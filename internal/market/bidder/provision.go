package bidder

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
)

const abandonTimeout = 30 * time.Second

type provisionRequest struct {
	rfpURI string

	rfpCID string

	vmURI string

	vmCID string

	acceptURI string

	acceptCID string

	bidURI string

	bidCID string

	requesterDID string

	vmPayload market.ResolvedRecord

	bidConfig market.StrongRef

	bidConfigValue map[string]any

	owner *metav1.OwnerReference
}

type vmRecord struct {
	CPUs int32 `json:"cpus"`

	Mem string `json:"mem"`

	Disk string `json:"disk"`

	Role string `json:"role"`

	UserData string `json:"user_data"`
}

func parseVMRecord(value map[string]any) (vmRecord, error) {
	var out vmRecord
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("bidder: the RFP payload is not a compute.vm record: %w", err)
	}
	out.CPUs = cmp.Or(out.CPUs, 2)
	if strings.TrimSpace(out.Mem) == "" {
		out.Mem = "4G"
	}
	switch {
	case out.Role == "":
		return out, errors.New("bidder: the RFP payload names no vm.role")
	case out.CPUs < 1:
		return out, fmt.Errorf("bidder: the RFP payload names %d cpus", out.CPUs)
	case strings.TrimSpace(out.UserData) == "":
		return out, errors.New("bidder: the RFP payload carries no user_data; a guest without it is unreachable")
	}
	return out, nil
}

func (b *Bidder) owns(handle string) bool {
	return handle == b.cfg.ProviderHandle
}

func (b *Bidder) provision(ctx context.Context, req provisionRequest) (string, error) {
	if b.vault == nil {
		return "", errors.New("bidder: the compute provider has no guest vault")
	}
	if b.cfg.GuestURL == "" {
		return "", errors.New("bidder: the compute provider has no guest identity issuer URL")
	}
	record, err := parseVMRecord(req.vmPayload.Value)
	if err != nil {
		return "", err
	}
	acceptKey := b.guests.AcceptKey(req.acceptURI, req.acceptCID)
	if acceptKey != "" {
		reuse, ok := b.vms.Find(func(vm v1alpha1.ComputeVM) bool {
			return b.owns(vm.Spec.Owner.Handle) && b.guests.AcceptKeyOf(&vm) == acceptKey && vm.DeletionTimestamp == nil &&
				!b.guests.Releasing(&vm) && !b.guests.Ended(&vm)
		})
		if ok {
			b.log.Info("a guest ComputeVM is reused for the accepted contract", "computevm", reuse.Name, "accept", req.acceptURI)
			return reuse.Name, nil
		}
	}
	if err := b.checkCapacity(); err != nil {
		return "", err
	}
	name, err := vmNameForAccept(req.requesterDID, acceptKey)
	if err != nil {
		return "", err
	}
	secret, secretHash, err := b.guests.LoginSecret(acceptKey)
	if err != nil {
		return "", err
	}
	issuer := strings.TrimRight(b.cfg.GuestURL, "/")
	bundle := map[string]any{
		"$type":               market.AcceptNSID,
		"accept":              market.NewStrongRef(req.acceptURI, req.acceptCID).Map(),
		"rfp":                 market.NewStrongRef(req.rfpURI, req.rfpCID).Map(),
		"bid":                 nil,
		"bid_config":          map[string]any{"uri": req.bidConfig.URI, "cid": req.bidConfig.CID, "value": req.bidConfigValue},
		"vm":                  map[string]any{"uri": req.vmURI, "cid": req.vmCID, "value": req.vmPayload.Value},
		"guest_onnetwork_url": issuer + guestnet.EventsPath,
	}
	if req.bidURI != "" {
		bundle["bid"] = market.NewStrongRef(req.bidURI, req.bidCID).Map()
	}
	withBundle, err := guestseed.InjectAcceptBundle(record.UserData, bundle, guestseed.AcceptPathVM)
	if err != nil {
		return "", err
	}
	hostURL, hostPort := b.GuestHostURL()
	userData, err := guestseed.Provision(guestseed.ProvisionOptions{
		UserData: withBundle,
		Login: &guestseed.GuestLoginOptions{
			URL:        issuer + guestseed.GuestLoginRoute,
			Role:       name,
			BindSecret: secret,
			Mount:      b.cfg.GuestLoginMount,
		},
		AcceptPath:     guestseed.AcceptPathVM,
		FallbackURL:    b.cfg.GuestURL,
		BearerAudience: b.guests.Audience(name),
		Reporter:       true,
		BaseURL:        true,
		HostAddress:    v1alpha1.GuestHostAddress,
		TLSPort:        b.cfg.GuestTLSPort,
		HostURL:        hostURL,
		Downloads:      b.cfg.GuestDownloads,
	})
	if err != nil {
		return "", err
	}
	path, entry := b.guests.UserData(name, userData)
	if err := b.vault.Write(ctx, b.cfg.GuestVaultMount, path, entry); err != nil {
		return "", fmt.Errorf("bidder: writing the guest user data: %w", err)
	}
	vm, err := b.guests.ComputeVM(GuestVM{
		Name:             name,
		Handle:           b.cfg.ProviderHandle,
		Shape:            v1alpha1.ComputeVMShape{Cpus: record.CPUs, Mem: record.Mem, Disk: record.Disk, Role: record.Role},
		RequesterDID:     req.requesterDID,
		VaultServer:      b.cfg.GuestVaultServer,
		VaultMount:       b.cfg.GuestVaultMount,
		StopGraceSeconds: b.cfg.StopGraceSeconds,
		Login:            &v1alpha1.ComputeVMGuestLogin{Mount: b.cfg.GuestLoginMount, BindSecretSha256: secretHash},
		Accept:           commonv1.StrongRef{URI: req.acceptURI, CID: req.acceptCID},
		HostPorts:        b.guestHostPorts(userData, hostPort),
		LANUDP:           b.cfg.GuestLANUDP,
		HostUDP:          b.cfg.GuestHostUDP,
		Owner:            req.owner,
	})
	if err != nil {
		return "", err
	}
	created := true
	if err := b.intents.CreateComputeVM(ctx, *vm); err != nil {
		if !errors.Is(err, contract.ErrExists) {
			return "", fmt.Errorf("bidder: creating the ComputeVM: %w", err)
		}
		existing, found := b.awaitVM(ctx, name)
		if !found || b.guests.AcceptKeyOf(&existing) != acceptKey || !b.owns(existing.Spec.Owner.Handle) {
			return "", fmt.Errorf("bidder: creating the ComputeVM: %w", err)
		}
		created = false
	}
	if err := b.registerGrant(ctx, name, req.requesterDID, record.Role); err != nil {
		if created {
			b.abandon(ctx, name)
		}
		return "", err
	}
	b.log.Info("a guest ComputeVM is applied", "computevm", name, "cpus", record.CPUs, "role", record.Role, "requester", req.requesterDID)
	return name, nil
}

func (b *Bidder) GuestHostURL() (string, int) {
	if b.cfg.GuestLocalAddress == nil {
		return "", 0
	}
	raw := strings.TrimSpace(b.cfg.GuestLocalAddress())
	if raw == "" {
		return "", 0
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return "", 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", 0
	}
	return "http://" + net.JoinHostPort(guestnet.HostAddress, strconv.Itoa(port)), port
}

func (b *Bidder) awaitVM(ctx context.Context, name string) (v1alpha1.ComputeVM, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, AwaitTimeout)
	defer cancel()
	var vm v1alpha1.ComputeVM
	found := false
	_ = b.vms.Await(waitCtx, func() bool {
		vm, found = b.vms.Get(name)
		return found
	})
	return vm, found
}

func (b *Bidder) checkCapacity() error {
	limit := b.cfg.MaxConcurrentGuests
	if limit <= 0 {
		return nil
	}
	live := len(b.vms.Filter(func(vm v1alpha1.ComputeVM) bool {
		return vm.DeletionTimestamp == nil && vm.Spec.Owner.Handle == b.cfg.ProviderHandle && !b.guests.Ended(&vm) && !b.guests.Releasing(&vm)
	}))
	if int32(live) >= limit {
		return fmt.Errorf("bidder: the %d guest quota is full", limit)
	}
	return nil
}

func (b *Bidder) registerGrant(ctx context.Context, name, requesterDID, role string) error {
	grant, err := b.guests.Grant(b.cfg.DID, requesterDID, role, name)
	if err != nil {
		return err
	}
	if err := b.intents.CreateGuestIdentityGrant(ctx, grant); err != nil && !errors.Is(err, contract.ErrExists) {
		return fmt.Errorf("bidder: writing the guest identity grant: %w", err)
	}
	return nil
}

func (b *Bidder) abandon(ctx context.Context, name string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonTimeout)
	defer cancel()
	if err := b.intents.DeleteComputeVM(cleanup, name); err != nil {
		b.log.Warn("releasing an unprovisioned ComputeVM failed", "computevm", name, "err", err)
	}
}

func vmNameForAccept(requesterDID, acceptKey string) (string, error) {
	prefix := vmNamePrefix(requesterDID)
	if acceptKey == "" {
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("bidder: drawing a ComputeVM name: %w", err)
		}
		return prefix + "-" + hex.EncodeToString(buf), nil
	}
	sum := sha256.Sum256([]byte(acceptKey))
	return prefix + "-" + hex.EncodeToString(sum[:4]), nil
}

func vmNamePrefix(requesterDID string) string {
	parts := strings.Split(requesterDID, ":")
	short := strings.ToLower(parts[len(parts)-1])
	var clean strings.Builder
	for _, r := range short {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			clean.WriteRune(r)
		}
	}
	text := strings.Trim(clean.String(), "-._")
	if len(text) > 12 {
		text = text[:12]
	}
	return "fc-" + cmp.Or(text, "guest")
}

func (b *Bidder) guestHostPorts(userData string, hostPort int) []int32 {
	ports := guestnet.HostPorts(append(append(slices.Clone(b.cfg.GuestHostPorts), hostPort), guestseed.HostPortsOf(userData, b.cfg.GuestTLSPort)...))
	out := make([]int32, 0, len(ports))
	for _, p := range ports {
		out = append(out, int32(p))
	}
	return out
}

package bidder

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
)

type VMStore interface {
	Get(ctx context.Context, name string) (*v1alpha1.ComputeVM, error)

	Create(ctx context.Context, vm *v1alpha1.ComputeVM) error

	Delete(ctx context.Context, name string) error

	List(ctx context.Context) ([]v1alpha1.ComputeVM, error)

	PatchMarket(ctx context.Context, name string, patch v1alpha1.ComputeVMMarket) error

	PatchContract(ctx context.Context, name string, contract v1alpha1.ComputeVMContract) error

	PatchAnnotations(ctx context.Context, name string, annotations map[string]string) error
}

type vmWaiter interface {
	WaitRunning(ctx context.Context, name string, timeout time.Duration) (*v1alpha1.ComputeVM, error)

	WaitAbsent(ctx context.Context, name string, timeout time.Duration) error
}

type BidStore interface {
	Create(ctx context.Context, bid *v1alpha1.Bid) error

	Get(ctx context.Context, name string) (*v1alpha1.Bid, error)

	List(ctx context.Context) ([]v1alpha1.Bid, error)

	PatchSpec(ctx context.Context, name string, spec v1alpha1.BidSpec) error

	PatchStatus(ctx context.Context, name string, status v1alpha1.BidStatus) error
}

type GrantStore interface {
	Create(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error
}

type GuestVault interface {
	WriteKV(ctx context.Context, mount, path string, data map[string]any) error

	ReadKV(ctx context.Context, mount, path string) (map[string]any, error)
}

type ProvisionRequest struct {
	RFPURI string

	RFPCID string

	VMURI string

	VMCID string

	AcceptURI string

	AcceptCID string

	BidURI string

	BidCID string

	RequesterDID string

	SubmitEventURL string

	VMPayload market.ResolvedRecord

	BidConfig market.StrongRef

	BidConfigValue map[string]any
}

type ProvisionResult struct {
	Name string

	VM *v1alpha1.ComputeVM

	UserData string
}

type ComputeProvider struct {
	Config ProviderConfig

	VMs VMStore

	Grants GrantStore

	Vault GuestVault

	DID string

	IssuerURL string

	Log *slog.Logger

	Now func() time.Time

	WaitForRunning bool
}

func (p *ComputeProvider) log() *slog.Logger {
	if p == nil || p.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return p.Log
}

func (p *ComputeProvider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
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
	raw, err := jsonMarshal(value)
	if err != nil {
		return out, err
	}
	if err := jsonUnmarshal(raw, &out); err != nil {
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

func (p *ComputeProvider) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	if p == nil || p.VMs == nil {
		return ProvisionResult{}, errors.New("bidder: the compute provider has no ComputeVM store")
	}
	if p.Vault == nil {
		return ProvisionResult{}, errors.New("bidder: the compute provider has no guest vault")
	}
	if p.DID == "" {
		return ProvisionResult{}, errors.New("bidder: the compute provider has no bidder DID")
	}
	if p.IssuerURL == "" {
		return ProvisionResult{}, errors.New("bidder: the compute provider has no guest identity issuer URL")
	}
	record, err := parseVMRecord(req.VMPayload.Value)
	if err != nil {
		return ProvisionResult{}, err
	}
	acceptKey := computevm.AcceptRef{URI: req.AcceptURI, CID: req.AcceptCID}.Key()
	if acceptKey != "" {
		vms, err := p.VMs.List(ctx)
		if err != nil {
			return ProvisionResult{}, fmt.Errorf("bidder: looking for the ComputeVM of an accepted contract: %w", err)
		}
		for i := range vms {
			existing := &vms[i]
			if !p.owns(existing.Spec.Owner.Handle) || computevm.AcceptKeyOf(existing) != acceptKey {
				continue
			}
			if existing.DeletionTimestamp != nil || computevm.Releasing(existing) || computevm.Ended(existing) {
				continue
			}
			result := ProvisionResult{Name: existing.Name, VM: existing}
			reuse := true
			if p.WaitForRunning && existing.Status.Phase != v1alpha1.ComputeVMPhaseRunning {
				if waiter, ok := p.VMs.(vmWaiter); ok {
					running, waitErr := waiter.WaitRunning(ctx, existing.Name, p.Config.BootTimeout)
					switch {
					case waitErr == nil:
						if running != nil {
							result.VM = running
						}
					case running != nil && computevm.Ended(running):
						reuse = false
					default:
						return ProvisionResult{}, waitErr
					}
				}
			}
			if reuse {
				p.log().Info("a guest ComputeVM is reused for the accepted contract", "computevm", existing.Name, "accept", req.AcceptURI)
				return result, nil
			}
		}
	}
	if err := p.checkCapacity(ctx); err != nil {
		return ProvisionResult{}, err
	}
	name, err := newVMNameForAccept(req.RequesterDID, acceptKey)
	if err != nil {
		return ProvisionResult{}, err
	}
	secret, secretHash, err := guestLoginSecret(acceptKey)
	if err != nil {
		return ProvisionResult{}, err
	}
	mount := p.Config.GuestLoginMount
	mount = cmp.Or(mount, DefaultGuestLoginMount)
	acceptPath := p.Config.AcceptPath
	acceptPath = cmp.Or(acceptPath, guestseed.AcceptPathVM)
	bundle := map[string]any{
		"$type":               market.AcceptNSID,
		"accept":              market.NewStrongRef(req.AcceptURI, req.AcceptCID).Map(),
		"rfp":                 market.NewStrongRef(req.RFPURI, req.RFPCID).Map(),
		"bid":                 nil,
		"bid_config":          map[string]any{"uri": req.BidConfig.URI, "cid": req.BidConfig.CID, "value": req.BidConfigValue},
		"vm":                  map[string]any{"uri": req.VMURI, "cid": req.VMCID, "value": req.VMPayload.Value},
		"guest_onnetwork_url": strings.TrimRight(p.IssuerURL, "/") + guestnet.OnNetworkPath,
	}
	if req.BidURI != "" {
		bundle["bid"] = market.NewStrongRef(req.BidURI, req.BidCID).Map()
	}
	withBundle, err := guestseed.InjectAcceptBundle(record.UserData, bundle, acceptPath)
	if err != nil {
		return ProvisionResult{}, err
	}
	userData, err := guestseed.Provision(guestseed.ProvisionOptions{
		UserData: withBundle,
		Login: &guestseed.GuestLoginOptions{
			URL:        strings.TrimRight(p.IssuerURL, "/") + guestseed.GuestLoginRoute,
			Role:       name,
			BindSecret: secret,
			Mount:      mount,
		},
		AcceptPath:     acceptPath,
		FallbackURL:    p.IssuerURL,
		BearerAudience: guestidentity.GuestAudience(guestidentity.KCPAPI, name),
		Reporter:       true,
		BaseURL:        true,
		HostAddress:    p.Config.GuestHostAddress,
		TLSPort:        p.Config.GuestTLSPort,
	})
	if err != nil {
		return ProvisionResult{}, err
	}
	path := computevm.UserDataPath(name)
	if err := p.Vault.WriteKV(ctx, p.Config.GuestVaultMount, path, computevm.UserDataEntry(userData)); err != nil {
		return ProvisionResult{}, fmt.Errorf("bidder: writing the guest user data: %w", err)
	}
	shape := v1alpha1.ComputeVMShape{Cpus: record.CPUs, Mem: record.Mem, Disk: record.Disk, Role: record.Role}
	vm, err := computevm.ComputeVMFor(computevm.VMInput{
		Name:             name,
		Handle:           p.Config.Handle,
		VM:               shape,
		RequesterDID:     req.RequesterDID,
		Vault:            computevm.GuestVaultRef{Server: p.Config.GuestVaultServer, Mount: p.Config.GuestVaultMount},
		ReuseStale:       p.Config.ReuseStale,
		StopGraceSeconds: p.Config.StopGraceSeconds,
		GuestLogin:       &v1alpha1.ComputeVMGuestLogin{Mount: mount, BindSecretSha256: secretHash},
		Accept:           &computevm.AcceptRef{URI: req.AcceptURI, CID: req.AcceptCID},
	})
	if err != nil {
		return ProvisionResult{}, err
	}
	created := true
	if err := p.VMs.Create(ctx, vm); err != nil {
		if !isAlreadyExists(err) {
			return ProvisionResult{}, fmt.Errorf("bidder: creating the ComputeVM: %w", err)
		}
		existing, getErr := p.VMs.Get(ctx, name)
		if getErr != nil {
			return ProvisionResult{}, fmt.Errorf("bidder: creating the ComputeVM: %w", err)
		}
		if computevm.AcceptKeyOf(existing) != acceptKey || !p.owns(existing.Spec.Owner.Handle) {
			return ProvisionResult{}, fmt.Errorf("bidder: creating the ComputeVM: %w", err)
		}
		created = false
		vm = existing
	}
	result := ProvisionResult{Name: name, VM: vm, UserData: userData}
	if err := p.registerGrant(ctx, name, req.RequesterDID, record.Role); err != nil {
		if created {
			p.abandon(ctx, name)
		}
		return ProvisionResult{}, err
	}
	if p.WaitForRunning {
		if waiter, ok := p.VMs.(vmWaiter); ok {
			running, err := waiter.WaitRunning(ctx, name, p.Config.BootTimeout)
			if err != nil {
				if created {
					p.abandon(ctx, name)
				}
				return ProvisionResult{}, err
			}
			result.VM = running
		}
	}
	p.log().Info("a guest ComputeVM is applied", "computevm", name, "cpus", record.CPUs, "role", record.Role, "requester", req.RequesterDID)
	return result, nil
}

func (p *ComputeProvider) checkCapacity(ctx context.Context) error {
	if p.Config.MaxConcurrentGuests <= 0 {
		return nil
	}
	vms, err := p.VMs.List(ctx)
	if err != nil {
		return fmt.Errorf("bidder: checking the guest quota: %w", err)
	}
	live := int32(0)
	for i := range vms {
		vm := &vms[i]
		if vm.DeletionTimestamp != nil || vm.Spec.Owner.Handle != p.Config.Handle {
			continue
		}
		if computevm.Ended(vm) || computevm.Releasing(vm) {
			continue
		}
		live++
	}
	if live >= p.Config.MaxConcurrentGuests {
		return fmt.Errorf("bidder: the %d guest quota is full", p.Config.MaxConcurrentGuests)
	}
	return nil
}

func (p *ComputeProvider) registerGrant(ctx context.Context, name, requesterDID, role string) error {
	if p.Grants == nil {
		return nil
	}
	subject, err := guestidentity.SubjectFor(p.DID, requesterDID, role)
	if err != nil {
		return err
	}
	grant := guestidentity.GrantFor(guestidentity.GrantSubject{
		ComputeVM:    name,
		RequesterDID: requesterDID,
		Subject:      subject,
	}, guestidentity.OwnWish(name))
	if err := p.Grants.Create(ctx, grant); err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("bidder: writing the guest identity grant: %w", err)
	}
	return nil
}

func (p *ComputeProvider) BindContract(ctx context.Context, name string, contract v1alpha1.ComputeVMContract) error {
	if p == nil || p.VMs == nil {
		return errors.New("bidder: the compute provider has no ComputeVM store")
	}
	return p.VMs.PatchContract(ctx, name, contract)
}

func (p *ComputeProvider) Destroy(ctx context.Context, name string) error {
	if p == nil || p.VMs == nil {
		return errors.New("bidder: the compute provider has no ComputeVM store")
	}
	if err := p.VMs.Delete(ctx, name); err != nil {
		return err
	}
	if waiter, ok := p.VMs.(vmWaiter); ok {
		return waiter.WaitAbsent(ctx, name, p.Config.ReleaseTimeout)
	}
	return nil
}

func (p *ComputeProvider) abandon(ctx context.Context, name string) {
	if p == nil || p.VMs == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := p.VMs.Delete(cleanup, name); err != nil {
		p.log().Warn("releasing an unprovisioned ComputeVM failed", "computevm", name, "err", err)
	}
}

func (p *ComputeProvider) owns(handle string) bool {
	return p != nil && handle == p.Config.Handle
}

func guestLoginSecret(acceptKey string) (string, string, error) {
	if acceptKey != "" {
		secret, hash := computevm.GuestLoginSecretFor(acceptKey)
		return secret, hash, nil
	}
	return computevm.GuestLoginSecret()
}

func newVMNameForAccept(requesterDID, acceptKey string) (string, error) {
	prefix, err := vmNamePrefix(requesterDID)
	if err != nil {
		return "", err
	}
	if acceptKey == "" {
		return newVMName(requesterDID)
	}
	sum := sha256.Sum256([]byte(acceptKey))
	return prefix + "-" + hex.EncodeToString(sum[:4]), nil
}

func newVMName(requesterDID string) (string, error) {
	prefix, err := vmNamePrefix(requesterDID)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("bidder: drawing a ComputeVM name: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buf), nil
}

func vmNamePrefix(requesterDID string) (string, error) {
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
	text = cmp.Or(text, "guest")
	return "fc-" + text, nil
}

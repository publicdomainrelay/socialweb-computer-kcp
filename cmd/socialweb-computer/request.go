package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketnetwork"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appcli"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/logx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/webrtcproxy"
)

const (
	requestSSHReady     = time.Minute
	requestIdentityWait = 5 * time.Minute
	requestSummaryRead  = 10 * time.Second
	requestAttachProbe  = 10 * time.Second
	requestSSHRetryRun  = 20 * time.Second
	transportIroh       = "iroh"
	transportWebRTC     = "webrtc"
	sshPort             = 22
	sshConnectionFailed = 255
)

var requestOptions = []string{"network", "name", "secrets", "tunnel", "agent", "association-ref", "hold", "exec", "skip-ssh", "contract-timeout", "state-dir",
	"ready-timeout", "network-timeout", "kcp-path", "kine-path", "bao-path", "dumbpipe-path", "asset-cache", "transport", "tag", "log-level"}

var requestCreateOptions = append(append([]string{}, requestOptions...), "identity")

type requestRun struct {
	a               *app
	vals            clicfg.Values
	stateRoot       string
	inv             request.Invocation
	hold            bool
	skipSSH         bool
	exec            string
	secrets         string
	transport       string
	tag             string
	target          string
	root            v1alpha1.MarketRequesterSpec
	contract        v1alpha1.ComputeContractSpec
	appOptions      []v1alpha1.ApplicationOption
	contractTimeout time.Duration
	networkTimeout  time.Duration
	summary         request.Summary
	kcp             *request.Kcp
	standalone      devinstance.Layout
	instanceDir     string
	resuming        bool
	cleanupOnly     bool
	identity        *request.IdentityStore
	identityState   *request.Identity
	identityObject  *v1alpha1.MarketIdentity
	expectedPDSURL  string
}

func (r *requestRun) openIdentity(name string) error {
	idName := strings.TrimSpace(r.vals.String("identity"))
	if idName == "" {
		return nil
	}
	if r.inv.Mode == request.ModeNetwork {
		return errors.New("--identity is for a standalone invocation; a market network places its child before its state dir exists")
	}
	if r.root.Identity.Mode != "" && r.root.Identity.Mode != v1alpha1.MarketIdentityModeLocalRepo {
		return fmt.Errorf("--identity needs the localRepo identity mode, not %s", r.root.Identity.Mode)
	}
	if r.root.Identity.PdsURL != "" {
		return errors.New("--identity does not take --atproto-pds-url")
	}
	store, err := request.OpenIdentity(r.stateRoot, idName)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		store.Close()
		return err
	}
	id, err := store.Load()
	if err != nil {
		return fail(err)
	}
	if held, err := store.PDSLockHeld(); err != nil {
		return fail(err)
	} else if held {
		return fail(fmt.Errorf("the identity %s is in use by a running PDS; release or stop its invocation first", idName))
	}
	if id != nil {
		if id.DID == "" {
			accounts, err := request.ReadAccounts(store.PDSDir())
			if err != nil {
				return fail(err)
			}
			switch len(accounts) {
			case 1:
				id.DID, id.Handle = accounts[0].DID, accounts[0].Handle
			case 0:
			default:
				return fail(fmt.Errorf("the identity %s PDS holds %d accounts; one identity serves one account", idName, len(accounts)))
			}
		}
		if id.DID == "" && id.PDSPublicURL != "" {
			return fail(fmt.Errorf("the identity %s names a PDS URL but no DID; the store is partial", idName))
		}
		if id.Handle != "" && r.root.Identity.Handle != "" && !strings.EqualFold(r.root.Identity.Handle, id.Handle) {
			return fail(fmt.Errorf("the identity %s uses handle %s, not %s", idName, id.Handle, r.root.Identity.Handle))
		}
		r.root.Identity.Mode = v1alpha1.MarketIdentityModeLocalRepo
		r.root.Identity.Did = id.DID
		if id.Handle != "" {
			r.root.Identity.Handle = id.Handle
		}
		switch v1alpha1.AssociationMode(id.Association) {
		case v1alpha1.AssociationSkip:
			r.root.Identity.Association = v1alpha1.AssociationSkip
		case v1alpha1.AssociationRequireApproval:
			r.root.Identity.Association = v1alpha1.AssociationRequireApproval
		default:
			return fail(fmt.Errorf("the identity %s has association %q", idName, id.Association))
		}
		r.root.Identity.ApprovedAssociations = append([]string(nil), id.ApprovedAssociations...)
		if r.root.Identity.Association == v1alpha1.AssociationSkip && r.root.Identity.AssociationRef != nil {
			return fail(fmt.Errorf("the identity %s has association Skip; --association-ref needs a RequireApproval identity", idName))
		}
		if r.root.Identity.Association == v1alpha1.AssociationRequireApproval && len(r.root.Identity.ApprovedAssociations) == 0 && r.root.Identity.AssociationRef == nil {
			return fail(fmt.Errorf("the identity %s is RequireApproval with no approved operator; pass --association-ref at://<authority>/<collection>/<rkey>#<cid>, or use --skip-qr to create a skip identity", idName))
		}
		if id.PLCDirectoryURL != "" && r.root.Network != nil && r.root.Network.PLCDirectoryURL != "" &&
			!strings.EqualFold(strings.TrimRight(id.PLCDirectoryURL, "/"), strings.TrimRight(r.root.Network.PLCDirectoryURL, "/")) {
			return fail(fmt.Errorf("the identity %s uses PLC %s, not %s", idName, id.PLCDirectoryURL, r.root.Network.PLCDirectoryURL))
		}
		if id.DID != "" {
			rot, err := store.Validate(id.DID, id.PDSPublicURL != "")
			if err != nil {
				return fail(fmt.Errorf("the identity %s: %w", idName, err))
			}
			if id.RotationKeyDID != "" && rot != id.RotationKeyDID {
				return fail(fmt.Errorf("the identity %s rotation key is %s, not the recorded %s", idName, rot, id.RotationKeyDID))
			}
			id.RotationKeyDID = rot
		}
		r.identityState = id
	} else {
		if entries, err := os.ReadDir(store.PDSDir()); err == nil && len(entries) > 0 {
			return fail(fmt.Errorf("the identity %s has PDS state but no identity.json; refusing to adopt it", idName))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		association := v1alpha1.AssociationSkip
		if r.root.Identity.AssociationRef != nil {
			association = v1alpha1.AssociationRequireApproval
		} else if r.root.Identity.Association != v1alpha1.AssociationSkip {
			return fail(fmt.Errorf("a new identity needs -- --skip-qr or --association-ref at://<authority>/<collection>/<rkey>#<cid>"))
		}
		r.root.Identity.Association = association
		r.identityState = &request.Identity{Schema: request.IdentitySchema, Name: idName, Mode: "localRepo", Association: string(association)}
	}
	if err := store.EnsureDirs(); err != nil {
		return fail(err)
	}
	r.identityState.LastInvocation = &request.IdentityInvocation{Name: name, StateDir: request.InstanceStateDir(r.stateRoot, name)}
	if err := store.Save(r.identityState); err != nil {
		return fail(err)
	}
	r.expectedPDSURL = strings.TrimSpace(r.identityState.PDSPublicURL)
	r.appOptions = append(r.appOptions, v1alpha1.ApplicationOption{Name: "pds-state-dir", Value: store.PDSDir()})
	if r.expectedPDSURL != "" {
		r.appOptions = append(r.appOptions, v1alpha1.ApplicationOption{Name: "expected-pds-url", Value: r.expectedPDSURL})
	}
	r.root.Options = r.appOptions
	r.identity = store
	r.inv.Identity = idName
	r.summary.Identity = idName
	r.log("the identity is "+idName, "did", r.identityState.DID, "store", store.Dir())
	return nil
}

func (r *requestRun) linkIdentityExposure() error {
	if r.identity == nil {
		return nil
	}
	return r.identity.LinkExposure(request.InstanceStateDir(r.stateRoot, r.inv.Name))
}

func (r *requestRun) saveIdentityDID(obj *v1alpha1.MarketIdentity) {
	if r.identity == nil || r.identityState == nil || obj == nil {
		return
	}
	did := strings.TrimSpace(obj.Status.Did)
	if did == "" {
		return
	}
	handle := strings.TrimSpace(obj.Status.Handle)
	if r.identityState.DID == did && (handle == "" || r.identityState.Handle == handle) {
		return
	}
	r.identityState.DID = did
	if handle != "" {
		r.identityState.Handle = handle
	}
	if err := r.identity.Save(r.identityState); err != nil {
		r.a.log.Warn("writing the identity DID", "identity", r.identity.Name(), "err", err.Error())
	}
}

func (r *requestRun) finalizeIdentity() error {
	if r.identity == nil || r.identityState == nil {
		return nil
	}
	obj := r.identityObject
	if obj == nil {
		ids, err := r.kcp.Identities()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), requestSummaryRead)
		defer cancel()
		obj, err = ids.GetMarketIdentity(ctx, marketrequester.RequesterIdentity)
		if err != nil {
			return err
		}
	}
	did := strings.TrimSpace(obj.Status.Did)
	if did == "" {
		return errors.New("the requester identity reports no DID")
	}
	if r.identityState.DID != "" && r.identityState.DID != did {
		return fmt.Errorf("the requester DID is %s, not the identity's %s", did, r.identityState.DID)
	}
	r.identityState.DID = did
	r.identityState.Mode = "localRepo"
	if handle := strings.TrimSpace(obj.Status.Handle); handle != "" {
		r.identityState.Handle = handle
	}
	if pdsURL := strings.TrimSpace(obj.Status.PdsURL); pdsURL != "" {
		r.identityState.PDSPublicURL = pdsURL
	}
	r.identityState.Association = string(r.root.Identity.Association)
	r.identityState.ApprovedAssociations = append([]string(nil), r.root.Identity.ApprovedAssociations...)
	if r.root.Network != nil {
		r.identityState.PLCDirectoryURL = r.root.Network.PLCDirectoryURL
	}
	if rot, err := r.identity.ReadRotationKeyDID(); err == nil {
		r.identityState.RotationKeyDID = rot
	}
	if r.identityState.LastInvocation == nil {
		r.identityState.LastInvocation = &request.IdentityInvocation{Name: r.inv.Name, StateDir: request.InstanceStateDir(r.stateRoot, r.inv.Name)}
	}
	return r.identity.Save(r.identityState)
}

func splitWords(rest []string) ([]string, []string) {
	i := 0
	for i < len(rest) && !strings.HasPrefix(rest[i], "--") {
		i++
	}
	pass := rest[i:]
	if len(pass) > 0 && pass[0] == "--" {
		pass = pass[1:]
	}
	return rest[:i], pass
}

func (r *requestRun) log(msg string, args ...any) {
	r.a.log.Info(msg, append([]any{"request", r.inv.Name, "utc", time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func newRequestRun(a *app, vals clicfg.Values) (*requestRun, error) {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return nil, err
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	r := &requestRun{a: a, vals: vals, stateRoot: root, hold: vals.Bool("hold"), skipSSH: vals.Bool("skip-ssh"),
		exec: vals.String("exec"), secrets: vals.String("secrets"), tag: strings.TrimSpace(vals.String("tag")),
		target: strings.TrimSpace(vals.String("contract"))}
	switch r.transport = strings.ToLower(strings.TrimSpace(vals.String("transport"))); r.transport {
	case "", transportIroh:
		r.transport = transportIroh
	case transportWebRTC:
		if r.tag == "" {
			return nil, errors.New("transport webrtc: --tag names the webrtc tunnel")
		}
	default:
		return nil, fmt.Errorf("transport %q: want iroh or webrtc", r.transport)
	}
	if r.contract.Tunnels, err = parseTunnels(vals.String("tunnel")); err != nil {
		return nil, fmt.Errorf("tunnel: %w", err)
	}
	r.contract.Agent = agentOf(vals.String("agent"))
	if r.contractTimeout, err = time.ParseDuration(vals.String("contract-timeout")); err != nil {
		return nil, fmt.Errorf("contract-timeout: %w", err)
	}
	if r.networkTimeout, err = time.ParseDuration(vals.String("network-timeout")); err != nil {
		return nil, fmt.Errorf("network-timeout: %w", err)
	}
	return r, nil
}

func splitList(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *requestRun) passthrough(args []string, network bool) error {
	parsed, err := compose.ParseAppArgs(marketrequester.RequesterApp, args)
	if err != nil {
		return err
	}
	r.root = v1alpha1.MarketRequesterSpec{Profile: marketrequester.RequesterProfileDefault,
		Identity: v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo}}
	var nv v1alpha1.MarketNetworkValue
	nvSet := false
	for _, p := range parsed {
		switch p.Name {
		case "identity", "pds-state-dir", "expected-pds-url":
			return fmt.Errorf("--%s is reserved; pass global flags before --", p.Name)
		}
		perApp := p.To == "spec.secretsFallback" || p.To == "spec.comms.mode" || p.To == "spec.comms.eventSourceRefs" || strings.HasPrefix(p.To, "spec.requesterIngress")
		if network && !perApp && (strings.HasPrefix(p.To, compose.MapToSpec) || strings.HasPrefix(p.To, compose.MapToNetwork)) {
			return fmt.Errorf("--%s maps to %s, which the market network sets for its child applications", p.Name, p.To)
		}
		switch {
		case p.To == compose.MapToOptions:
			r.appOptions = append(r.appOptions, v1alpha1.ApplicationOption{Name: p.Name, Value: p.Value})
		case p.To == compose.MapToCLI:
			switch p.Name {
			case "exec":
				r.exec = p.Value
			case "hold", "keep-vm":
				r.hold = p.Value != "false"
			case "skip-ssh":
				r.skipSSH = p.Value != "false"
			}
		case p.To == "spec.identity.mode":
			switch p.Name {
			case "atproto-oauth":
				r.root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthBrowser
			case "atproto-oauth-qr":
				r.root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthQr
			default:
				r.root.Identity.Mode = v1alpha1.MarketIdentityMode(p.Value)
			}
		case p.To == "spec.identity.handle":
			r.root.Identity.Handle = p.Value
		case p.To == "spec.identity.pdsUrl":
			r.root.Identity.PdsURL = p.Value
		case p.To == "spec.identity.association":
			if p.Value != "false" {
				r.root.Identity.Association = v1alpha1.AssociationSkip
			}
		case p.To == "spec.bidderDids":
			r.root.BidderDids = append(r.root.BidderDids, splitList(p.Value)...)
		case strings.HasPrefix(p.To, "spec.requesterIngress"):
			var applied bool
			var err error
			if r.root.RequesterIngress, applied, err = applyIngressOption(r.root.RequesterIngress, p.Name, p.Value); err != nil {
				return err
			}
			if !applied {
				return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
			}
		case strings.HasPrefix(p.To, "spec.ingress"):
			var applied bool
			var err error
			if r.root.Ingress, applied, err = applyIngressOption(r.root.Ingress, p.Name, p.Value); err != nil {
				return err
			}
			if !applied {
				return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
			}
		case p.To == "spec.secretsFallback":
			switch fallback := v1alpha1.SecretsFallback(strings.TrimSpace(p.Value)); fallback {
			case v1alpha1.SecretsFallbackNone, v1alpha1.SecretsFallbackSealed:
				r.root.SecretsFallback = fallback
			default:
				return fmt.Errorf("--secrets-fallback %q: want None or Sealed", p.Value)
			}
		case strings.HasPrefix(p.To, "spec.comms"):
			var applied bool
			var err error
			if r.root.Comms, applied, err = applyCommsOption(r.root.Comms, p.Name, p.Value); err != nil {
				return err
			}
			if !applied {
				return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
			}
		case strings.HasPrefix(p.To, compose.MapToNetwork):
			nvSet = true
			switch p.To {
			case "network.plcDirectoryUrl":
				nv.PLCDirectoryURL = p.Value
			case "network.ingressProxyHost":
				nv.IngressProxyHost = p.Value
			case "network.qrOrigin":
				nv.QrOrigin = p.Value
			case "network.hosts":
				for _, h := range splitList(p.Value) {
					name, addr, ok := strings.Cut(h, "=")
					if !ok {
						return fmt.Errorf("--network-hosts %q: want name=address,...", p.Value)
					}
					nv.Hosts = append(nv.Hosts, v1alpha1.NetworkHost{Name: name, Address: addr})
				}
			}
		case p.To == "contract.bidderDids":
			r.contract.BidderDids = append(r.contract.BidderDids, splitList(p.Value)...)
		case p.To == "contract.denyBidderDids":
			r.contract.DenyBidderDids = append(r.contract.DenyBidderDids, splitList(p.Value)...)
		case p.To == "contract.vmName":
			r.contract.VMName = p.Value
		case p.To == "contract.computeKind":
			switch kind := v1alpha1.ComputeKind(strings.TrimSpace(p.Value)); kind {
			case v1alpha1.ComputeKindVM, v1alpha1.ComputeKindDeno:
				r.contract.ComputeKind = kind
			default:
				return fmt.Errorf("--compute-kind %q: want vm or deno", p.Value)
			}
		case p.To == "contract.userData":
			b, err := os.ReadFile(p.Value)
			if err != nil {
				return fmt.Errorf("--user-data: %w", err)
			}
			r.contract.UserData = string(b)
		case p.To == "contract.sshAuthorizedKeys":
			r.contract.SshAuthorizedKeys = append(r.contract.SshAuthorizedKeys, splitList(p.Value)...)
		case p.To == "contract.policy":
			r.contract.Policy = p.Value
		case p.To == "contract.policyArgs":
			r.contract.PolicyArgs = p.Value
		case p.To == "contract.vmReadyTimeoutSec":
			n, err := strconv.Atoi(p.Value)
			if err != nil {
				return fmt.Errorf("--vm-ready-timeout-sec %q: %w", p.Value, err)
			}
			r.contract.VMReadyTimeoutSec = n
		case p.To == "contract.secretsRef":
			r.secrets = p.Value
		case p.To == "contract.agent":
			r.contract.Agent = agentOf(p.Value)
		case p.To == "contract.tunnels":
			tunnels, err := parseTunnels(p.Value)
			if err != nil {
				return fmt.Errorf("--tunnel: %w", err)
			}
			r.contract.Tunnels = append(r.contract.Tunnels, tunnels...)
		default:
			return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
		}
	}
	if !network {
		r.contract.Policy = cmp.Or(strings.TrimSpace(r.contract.Policy), requester.DefaultPolicyWorkflow)
	}
	if nvSet || !network {
		nv.PLCDirectoryURL = cmp.Or(nv.PLCDirectoryURL, "https://plc.directory")
		nv.IngressProxyHost = cmp.Or(nv.IngressProxyHost, "xrpc.fedproxy.com")
		r.root.Network = &nv
	}
	if !network && r.root.Ingress == nil {
		r.root.Ingress = compose.DefaultIngressConfig()
		if nv.IngressProxyHost != "" {
			r.root.Ingress.Xrpc.IngressProxyHost = nv.IngressProxyHost
		}
	}
	r.root.Options = r.appOptions
	return nil
}

func parseTunnels(value string) ([]v1alpha1.ComputeContractTunnel, error) {
	var out []v1alpha1.ComputeContractTunnel
	for _, item := range splitList(value) {
		parts := strings.Split(item, ":")
		if len(parts) < 2 || len(parts) > 4 {
			return nil, fmt.Errorf("%q: want <tag>:<port>[:<transport>][:shell[=<envFile>]] or <tag>:<port>:<envFile>", item)
		}
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%q: port: %w", item, err)
		}
		tunnel := v1alpha1.ComputeContractTunnel{Tag: parts[0], Port: port}
		rest := parts[2:]
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "/") && !isShellForm(rest[0]) {
			switch transport := v1alpha1.TunnelTransport(rest[0]); transport {
			case v1alpha1.TunnelTransportOpenTunnel, v1alpha1.TunnelTransportWebRTC:
				tunnel.Transport = transport
			default:
				return nil, fmt.Errorf("%q: transport %q is not %s or %s", item, rest[0], v1alpha1.TunnelTransportOpenTunnel, v1alpha1.TunnelTransportWebRTC)
			}
			rest = rest[1:]
		}
		switch {
		case len(rest) == 0:
		case len(rest) == 1 && strings.HasPrefix(rest[0], "/") && len(parts) == 3:
			tunnel.Shell = &v1alpha1.ComputeContractShell{EnvFile: rest[0]}
		case len(rest) == 1 && isShellForm(rest[0]):
			envFile, _ := strings.CutPrefix(strings.TrimPrefix(rest[0], "shell"), "=")
			tunnel.Shell = &v1alpha1.ComputeContractShell{EnvFile: envFile}
		default:
			return nil, fmt.Errorf("%q: want <tag>:<port>[:<transport>][:shell[=<envFile>]] or <tag>:<port>:<envFile>", item)
		}
		if tunnel.Shell != nil && tunnel.Transport == v1alpha1.TunnelTransportWebRTC {
			return nil, fmt.Errorf("%q: a wootty shell is served over opentunnel, not webrtc", item)
		}
		out = append(out, tunnel)
	}
	return out, nil
}

func isShellForm(part string) bool {
	return part == "shell" || strings.HasPrefix(part, "shell=")
}

func (r *requestRun) webrtcTunnel() error {
	if r.transport != transportWebRTC {
		return nil
	}
	for _, t := range r.contract.Tunnels {
		if t.Tag == r.tag {
			if t.Transport != v1alpha1.TunnelTransportWebRTC {
				return fmt.Errorf("--transport webrtc: the tunnel %s has transport %s, not webrtc", t.Tag, cmp.Or(string(t.Transport), string(v1alpha1.TunnelTransportOpenTunnel)))
			}
			return nil
		}
	}
	r.contract.Tunnels = append(r.contract.Tunnels, v1alpha1.ComputeContractTunnel{Tag: r.tag, Port: sshPort, Transport: v1alpha1.TunnelTransportWebRTC})
	return nil
}

func secretsApplied(c *v1alpha1.ComputeContract) bool {
	return c.Status.SecretsFetchedAt != "" || meta.IsStatusConditionTrue(c.Status.Conditions, v1alpha1.ComputeContractConditionSecretsApplied)
}

func readSecrets(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s is not a JSON array of {path, value}: %w", path, err)
	}
	for i, e := range entries {
		if !strings.HasPrefix(e.Path, "/") || strings.Contains(e.Path, "/../") {
			return nil, fmt.Errorf("%s: entry %d path %q is not an absolute path", path, i, e.Path)
		}
	}
	return b, nil
}

func (r *requestRun) networkLayout() (devinstance.Layout, devinstance.Record, error) {
	l, err := r.a.layoutOf(r.inv.Network, r.vals)
	if err != nil {
		return l, devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		return l, rec, fmt.Errorf("market network %s has no instance record (dev up --profile market-network first): %w", r.inv.Network, err)
	}
	if rec.Profile != bootstrap.ProfileMarketNetwork || rec.AdminKubeconfig == "" {
		return l, rec, fmt.Errorf("instance %s is not a market network (profile %s)", r.inv.Network, rec.Profile)
	}
	return l, rec, nil
}

func (r *requestRun) childRecord(netLayout devinstance.Layout, instance string) (devinstance.Record, error) {
	cl, err := devinstance.NewLayout(instance, filepath.Join(netLayout.StateDir, compose.AppsDir), filepath.Join(netLayout.RuntimeDir, compose.AppsDir))
	if err != nil {
		return devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(cl.Record, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func (r *requestRun) start(ctx context.Context, fresh bool) error {
	started := time.Now()
	var kubeconfig, workspace string
	switch r.inv.Mode {
	case request.ModeNetwork:
		netLayout, netRec, err := r.networkLayout()
		if err != nil {
			return err
		}
		if fresh {
			app := v1alpha1.NetworkApp{Name: r.inv.Name, Kind: v1alpha1.ApplicationKindMarketRequester,
				Profile: marketrequester.RequesterProfileNetwork, Options: r.appOptions, SecretsFallback: r.root.SecretsFallback,
				RequesterIngress: r.root.RequesterIngress}
			if r.root.Comms != nil {
				app.Components = v1alpha1.ComponentOverrides{compose.ComponentKindRequester: {Comms: r.root.Comms}}
			}
			if err := request.AddToNetwork(ctx, netRec.AdminKubeconfig, app); err != nil {
				return err
			}
		} else if err := request.SetStopped(ctx, netRec.AdminKubeconfig, r.inv.Name, false); err != nil {
			return err
		}
		r.log("the market network places the invocation in its workspace of the network's kcp", "network", r.inv.Network)
		app, err := request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
			return a.Instance != "" && a.KcpURL != "" && a.Message != marketnetwork.StoppedMessage && a.Phase != v1alpha1.ApplicationPhasePending
		}, func(a v1alpha1.NetworkAppStatus) {
			r.log("network child", "phase", string(a.Phase), "message", a.Message)
		})
		if err != nil {
			return err
		}
		rec, err := r.childRecord(netLayout, app.Instance)
		if err != nil {
			return err
		}
		r.instanceDir = rec.Layout.StateDir
		r.inv.Instance, r.summary.Instance, r.summary.KcpURL = app.Instance, app.Instance, rec.KcpURL
		kubeconfig, workspace = rec.AdminKubeconfig, rec.BaseWorkspace
	default:
		vals := r.vals.With("instance", r.inv.Name).With("profile", bootstrap.ProfileQuick).
			With("state-dir", request.Dir(r.stateRoot, r.inv.Name)).With(appRoleOpt, devinstance.RoleRequester)
		up, code := bringUp(ctx, r.a, vals, false)
		if code != exitOK {
			return fmt.Errorf("the invocation's kcp did not come up (exit %d)", code)
		}
		r.standalone, r.instanceDir = up.layout, up.layout.StateDir
		r.inv.Instance, r.summary.Instance, r.summary.KcpURL = up.Instance, up.Instance, up.KcpURL
		kubeconfig = up.AdminKubeconfig
	}
	k, err := request.OpenKcp(kubeconfig, workspace)
	if err != nil {
		return err
	}
	r.kcp = k.ForContract(r.target)
	if r.inv.Mode == request.ModeStandalone && fresh {
		if err := k.ApplyRoot(ctx, r.root); err != nil {
			return fmt.Errorf("applying the MarketRequester root object: %w", err)
		}
	}
	r.summary.KcpColdSeconds = time.Since(started).Seconds()
	r.summary.KcpRunning = true
	r.log("the invocation's kcp runs", "kcp", r.summary.KcpURL, "workspace", workspace, "instance", r.inv.Instance, "coldSeconds", r.summary.KcpColdSeconds)
	if err := r.signIn(ctx, fresh || r.resuming); err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	if err := r.kcp.WaitReady(ctx, r.networkTimeout, func(st v1alpha1.ApplicationStatus) {
		r.log("requester", "phase", string(st.Phase), "message", st.Message, "components", len(st.Components))
	}); err != nil {
		return err
	}
	return request.WriteInvocation(r.inv)
}

func (r *requestRun) stop(ctx context.Context, remove bool) {
	r.keepLogs()
	switch r.inv.Mode {
	case request.ModeNetwork:
		_, netRec, err := r.networkLayout()
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
			return
		}
		if remove {
			err = request.RemoveFromNetwork(ctx, netRec.AdminKubeconfig, r.inv.Name)
		} else {
			err = request.SetStopped(ctx, netRec.AdminKubeconfig, r.inv.Name, true)
		}
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
			return
		}
		if !remove {
			_, err = request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
				return a.Message == marketnetwork.StoppedMessage
			}, nil)
		} else {
			err = request.WaitNetworkGone(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout)
		}
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
		}
	default:
		l := r.standalone
		if l.ID == "" {
			vals := r.vals.With("instance", r.inv.Name).With("state-dir", request.Dir(r.stateRoot, r.inv.Name))
			var err error
			if l, err = r.a.layoutOf(r.inv.Name, vals); err != nil {
				r.a.log.Warn("stopping the invocation", "err", err.Error())
				return
			}
		}
		d := downInstance(ctx, r.a, l, !remove, true)
		if len(d.Remaining) > 0 {
			r.a.log.Warn("processes of the invocation's kcp remain", "pids", fmt.Sprint(d.Remaining))
		}
	}
	r.summary.KcpRunning = false
	r.log("the invocation's kcp is stopped", "stateKept", !remove)
}

func (r *requestRun) writeSummary(ctx context.Context, c *v1alpha1.ComputeContract) {
	r.summary.UpdatedAt = time.Now().UTC()
	if c != nil {
		r.summary.SecretsFetchedAt = c.Status.SecretsFetchedAt
		r.summary.Phase, r.summary.Message, r.summary.SecretsUrl = string(c.Status.Phase), c.Status.Message, c.Status.SecretsURL
		r.summary.Contract = request.SummaryOf(c)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestSummaryRead)
	defer cancel()
	if st, err := r.kcp.Root(ctx); err == nil && st.Did != "" {
		r.summary.Did = st.Did
	}
	if err := request.WriteSummary(r.stateRoot, r.summary); err != nil {
		r.a.log.Warn("writing summary.json", "err", err.Error())
	}
}

func (r *requestRun) signIn(ctx context.Context, untilAssociated bool) error {
	if err := r.kcp.WaitIdentityObject(ctx, r.networkTimeout); err != nil {
		return err
	}
	ids, err := r.kcp.Identities()
	if err != nil {
		return err
	}
	var in io.Reader
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		in = os.Stdin
	}
	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	deadline := time.AfterFunc(requestIdentityWait, func() {
		cancel(fmt.Errorf("the requester identity asked for no sign-in step and was not Ready within %s", requestIdentityWait))
	})
	defer deadline.Stop()
	watch := func(ctx context.Context, name string, each func(*v1alpha1.MarketIdentity) (bool, error)) (*v1alpha1.MarketIdentity, error) {
		return r.kcp.AwaitIdentity(ctx, name, func(obj *v1alpha1.MarketIdentity) (bool, error) {
			r.saveIdentityDID(obj)
			if !untilAssociated && obj != nil && obj.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingApproval {
				return true, nil
			}
			if err := associationRefRefused(obj); err != nil {
				return false, err
			}
			if waitsOnPerson(obj) && deadline.Stop() {
				r.log("run request: the requester identity waits on a person (a QR scan, a browser sign-in or the operator's association); waiting without a deadline until it is Ready, Ctrl-C stops and keeps the invocation's state",
					"identity", name, "phase", string(obj.Status.Phase))
			}
			return each(obj)
		})
	}
	id, err := appcli.IdentityRenderer{Client: ids, Watch: watch, Out: r.a.stderr, In: in, Hint: r.identityHint}.WaitReady(wctx, marketrequester.RequesterIdentity)
	if id != nil {
		r.identityObject = id
	}
	if cause := context.Cause(wctx); err != nil && cause != nil && ctx.Err() == nil {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

func waitsOnPerson(obj *v1alpha1.MarketIdentity) bool {
	if obj == nil {
		return false
	}
	switch obj.Status.Phase {
	case v1alpha1.MarketIdentityPhaseReady, v1alpha1.MarketIdentityPhaseFailed:
		return false
	}
	if obj.Status.PendingAction != nil {
		return true
	}
	switch obj.Status.Phase {
	case v1alpha1.MarketIdentityPhaseAwaitingScan, v1alpha1.MarketIdentityPhaseAwaitingAuthorization,
		v1alpha1.MarketIdentityPhaseAwaitingApproval, v1alpha1.MarketIdentityPhaseSessionExpired:
		return true
	}
	return false
}

func (r *requestRun) identityHint(obj *v1alpha1.MarketIdentity) string {
	if obj.Status.Phase != v1alpha1.MarketIdentityPhaseAwaitingApproval || obj.Status.PendingAction != nil {
		return ""
	}
	did := cmp.Or(obj.Status.Did, "<the requester DID above>")
	if obj.Spec.AssociationRef != nil {
		return associationRefHint(obj, did)
	}
	return strings.Join([]string{
		"The requester's DID " + did + " is a new localRepo DID with association RequireApproval. No bidder accepts it",
		"until the operator's verified association claim names it. The operator writes that claim from a checkout",
		"of socialweb-computer-kcp, with the operator account's credential file:",
		"  deno run -A --no-lock scripts/associate-bidder.ts --creds <operator credential file> --account <ACCOUNT> --requester-did " + did,
		"This invocation keeps waiting for it; Ctrl-C stops it before any contract is posted.",
		"The prod CLI requester signs in with an existing account instead: socialweb-computer run request ... -- --atproto-oauth-qr",
	}, "\n")
}

func leftHeld(c *v1alpha1.ComputeContract) bool {
	return c.Status.Phase != v1alpha1.ComputeContractHeld
}

func contractSettled(c *v1alpha1.ComputeContract) bool {
	switch c.Status.Phase {
	case v1alpha1.ComputeContractHeld, v1alpha1.ComputeContractFailed, v1alpha1.ComputeContractReleased:
		return c.Status.ObservedGeneration == c.Generation || c.Status.ObservedGeneration == 0
	}
	return false
}

func (r *requestRun) progress(c *v1alpha1.ComputeContract) {
	phase := ""
	if c.Status.Contract != nil {
		phase = string(c.Status.Contract.Phase)
	}
	r.log("contract", "phase", string(c.Status.Phase), "contractPhase", phase, "message", c.Status.Message)
}

func (r *requestRun) sshClient(ctx context.Context) (request.SSH, error) {
	s := request.SSH{Dir: filepath.Join(request.Dir(r.stateRoot, r.inv.Name), request.SSHDir)}
	if r.transport == transportWebRTC {
		proxy, err := r.proxyCommand()
		if err != nil {
			return s, err
		}
		s.ProxyCommand = proxy
		return s, nil
	}
	dp, err := r.a.devExe(ctx, r.vals, "dumbpipe", "dumbpipe-path")
	if err != nil {
		return s, err
	}
	s.Dumbpipe = dp.Path
	return s, nil
}

func (r *requestRun) proxyCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving this binary's own path for the ProxyCommand: %w", err)
	}
	words := []string{exe, "run", "request", "proxy", r.inv.Name, "--tag", r.tag, "--state-dir", r.stateRoot}
	if level := strings.TrimSpace(r.vals.String("log-level")); level != "" {
		words = append(words, "--log-level", level)
	}
	if r.target != "" {
		words = append(words, "--contract", r.target)
	}
	for i, w := range words {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'"'"'`) + "'"
	}
	return strings.Join(words, " "), nil
}

func (r *requestRun) ssh(ctx context.Context, c *v1alpha1.ComputeContract) (int, error) {
	started := time.Now()
	s, err := r.sshClient(ctx)
	if err != nil {
		return -1, err
	}
	t, err := s.Target(c)
	if err != nil {
		return -1, err
	}
	how := "its iroh ticket"
	if r.transport == transportWebRTC {
		how = "a WebRTC datachannel of tunnel " + r.tag + " (run request proxy)"
	}
	r.log("ssh to the guest over "+how+", StrictHostKeyChecking=yes against its reported host keys", "alias", t.Alias,
		"hostKeys", strings.Join(request.SummaryOf(c).HostKeys, ", "))
	held, stop := r.whileHeld(ctx)
	defer stop()
	if r.transport != transportWebRTC {
		err = s.WaitReady(held, t, requestSSHReady, func(attempt int, msg string) {
			r.log("ssh not ready yet", "attempt", attempt, "err", msg)
		})
	}
	if cause := context.Cause(held); err != nil && cause != nil && ctx.Err() == nil {
		return -1, cause
	}
	if err != nil {
		return -1, err
	}
	interactive := false
	if r.exec == "" {
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			r.log("no --exec and stdin is not a terminal: the guest answered ssh; no session opened")
			return 0, nil
		}
		interactive = true
	}
	code, err := r.runSSH(held, s, t, interactive)
	r.log("ssh returned", "transport", r.transport, "exit", code, "seconds", time.Since(started).Seconds())
	if cause := context.Cause(held); cause != nil && ctx.Err() == nil {
		r.log("ssh session ended because the contract left Held", "err", cause)
		return 0, nil
	}
	if err != nil {
		if cause := context.Cause(held); cause != nil && ctx.Err() == nil {
			r.log("ssh session ended because the contract left Held", "err", cause)
			return 0, nil
		}
		if current, cerr := r.kcp.AwaitContract(ctx, 15*time.Second, leftHeld); cerr == nil {
			r.log("ssh session ended because the contract left Held", "phase", current.Status.Phase, "err", err)
			return 0, nil
		}
	}
	if r.hold {
		if current, cerr := r.kcp.AwaitContract(ctx, 5*time.Second, leftHeld); cerr == nil {
			r.log("ssh session ended because the contract left Held", "phase", current.Status.Phase)
			return 0, nil
		}
	}
	r.log("ssh session ended", "exit", code)
	return code, err
}

func (r *requestRun) runSSH(ctx context.Context, s request.SSH, t request.Target, interactive bool) (int, error) {
	deadline := time.Now().Add(requestSSHReady)
	for attempt := 1; ; attempt++ {
		began := time.Now()
		code, err := s.Run(ctx, t, r.exec, interactive, os.Stdin, r.a.stdout, r.a.stderr)
		if r.transport != transportWebRTC || err != nil || code != sshConnectionFailed || time.Since(began) > requestSSHRetryRun ||
			time.Now().After(deadline) || ctx.Err() != nil {
			return code, err
		}
		r.log("ssh over webrtc did not connect; trying again", "attempt", attempt, "seconds", time.Since(began).Seconds())
		select {
		case <-ctx.Done():
			return code, nil
		case <-time.After(request.SSHPollInterval):
		}
	}
}

func (r *requestRun) whileHeld(ctx context.Context) (context.Context, func()) {
	held, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		if c, err := r.kcp.AwaitContract(held, 0, leftHeld); err == nil {
			cancel(fmt.Errorf("the contract left Held while ssh waited for the guest: phase %s: %s", c.Status.Phase, c.Status.Message))
		}
	})
	return held, func() {
		cancel(nil)
		wg.Wait()
	}
}

func (r *requestRun) release(ctx context.Context) (*v1alpha1.ComputeContract, error) {
	if err := r.kcp.Release(ctx); err != nil {
		return nil, fmt.Errorf("setting spec.release: %w", err)
	}
	r.log("asked the requester to release the contract (spec.release)")
	return r.kcp.WaitContract(ctx, r.contractTimeout, func(c *v1alpha1.ComputeContract) bool {
		return c.Status.Phase == v1alpha1.ComputeContractReleased ||
			(c.Status.Phase == v1alpha1.ComputeContractFailed && c.Spec.Release && c.Status.Contract != nil && c.Status.Contract.Release != nil)
	}, r.progress)
}

func (r *requestRun) carryOn(ctx context.Context) int {
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		r.a.log.Error("the contract did not settle", "err", err.Error())
		r.writeSummary(ctx, c)
		r.stop(context.WithoutCancel(ctx), false)
		r.a.printJSON(r.summary)
		return exitFailed
	}
	if c.Status.Phase == v1alpha1.ComputeContractHeld && c.Spec.SecretsRef != nil && !secretsApplied(c) {
		r.log("waiting for the guest to write its secrets (getSecrets, or the sealed fallback when enabled)")
		if fc, err := r.kcp.WaitContract(ctx, r.contractTimeout, func(c *v1alpha1.ComputeContract) bool {
			return secretsApplied(c) || c.Status.Phase != v1alpha1.ComputeContractHeld
		}, r.progress); err != nil {
			r.a.log.Error("the guest did not write its secrets", "err", err.Error())
		} else {
			c = fc
			cond := meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ComputeContractConditionSecretsApplied)
			reason := ""
			if cond != nil {
				reason = cond.Reason
			}
			r.log("the guest wrote its secrets", "via", reason, "fetchedAt", c.Status.SecretsFetchedAt)
		}
	}
	code := exitOK
	hasContract := c.Status.Contract != nil && c.Status.Contract.Receipt != nil
	if c.Status.Phase == v1alpha1.ComputeContractHeld && !r.skipSSH {
		ec, err := r.ssh(ctx, c)
		if err != nil {
			r.a.log.Error("ssh", "err", err.Error())
			code = exitFailed
		} else if ec != 0 {
			code = exitFailed
		}
	}
	if c.Status.Phase == v1alpha1.ComputeContractFailed && !hasContract {
		code = exitFailed
	}
	remove := c.Status.Phase == v1alpha1.ComputeContractReleased || !hasContract
	if !r.hold && hasContract && c.Status.Phase != v1alpha1.ComputeContractReleased {
		rc, err := r.release(context.WithoutCancel(ctx))
		if rc != nil {
			c = rc
		}
		if err != nil || c.Status.Phase != v1alpha1.ComputeContractReleased {
			r.a.log.Error("the release did not finish", "err", fmt.Sprint(err), "phase", string(c.Status.Phase))
			code = exitFailed
		} else {
			remove = true
		}
	}
	r.writeSummary(ctx, c)
	r.stop(context.WithoutCancel(ctx), remove)
	r.writeSummary(ctx, nil)
	r.a.printJSON(r.summary)
	return code
}

func runRequest(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	pos, pass := splitWords(rest)
	if len(pos) > 0 {
		return a.fail(exitRefused, "run request: unexpected %q; request-vm-ssh options go after --", strings.Join(pos, " "))
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return a.fail(exitRefused, "run request: %s is not on PATH; install the OpenSSH client (apt install openssh-client, dnf install openssh-clients)", tool)
		}
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	name := vals.String("name")
	if name == "" {
		name = "req-" + strings.ToLower(time.Now().UTC().Format("20060102t150405"))
	}
	if err := request.CheckName(name); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	if _, err := os.Stat(request.Dir(r.stateRoot, name)); err == nil {
		return a.fail(exitRefused, "run request: invocation %s exists under %s; run request resume|release %s", name, request.Root(r.stateRoot), name)
	}
	network := vals.String("network")
	if err := r.passthrough(pass, network != ""); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	if err := r.associationRef(network != ""); err != nil {
		return a.fail(exitRefused, "run request: --association-ref: %v", err)
	}
	if r.contract.ComputeKind == v1alpha1.ComputeKindDeno {
		r.skipSSH = true
	}
	if err := r.webrtcTunnel(); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	var bundle []byte
	if r.secrets != "" {
		if bundle, err = readSecrets(r.secrets); err != nil {
			return a.fail(exitRefused, "run request: --secrets: %v", err)
		}
	}
	if r.contract.Agent != nil {
		if bundle, err = withAgentPlaceholder(bundle, r.contract.Agent.EnvFile); err != nil {
			return a.fail(exitRefused, "run request: --agent: %v", err)
		}
	}
	r.inv = request.Invocation{Name: name, Mode: request.ModeStandalone, Network: network, StateRoot: r.stateRoot, CreatedAt: time.Now().UTC()}
	if network != "" {
		r.inv.Mode = request.ModeNetwork
	}
	r.summary = request.Summary{Name: name, Mode: r.inv.Mode, Network: network, CreatedAt: r.inv.CreatedAt}
	if err := r.openIdentity(name); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	defer func() {
		if r.identity != nil {
			r.identity.Close()
		}
	}()
	if !r.skipSSH {
		pub, _, err := request.EnsureKey(ctx, filepath.Join(request.Dir(r.stateRoot, name), request.SSHDir))
		if err != nil {
			return a.fail(exitFailed, "run request: %v", err)
		}
		r.contract.SshAuthorizedKeys = append([]string{pub}, r.contract.SshAuthorizedKeys...)
	}
	if err := request.WriteInvocation(r.inv); err != nil {
		return a.fail(exitFailed, "run request: %v", err)
	}
	if err := r.linkIdentityExposure(); err != nil {
		return a.fail(exitFailed, "run request: %v", err)
	}
	r.log("a fresh kcp for this invocation", "mode", r.inv.Mode, "network", network)
	if err := r.start(ctx, true); err != nil {
		r.writeSummaryQuiet()
		r.stop(context.WithoutCancel(ctx), false)
		if ctx.Err() != nil {
			return a.fail(exitFailed, "run request: stopped before the requester was Ready, so no contract was posted; the invocation's kcp is stopped and its state is kept in %s (delete that directory to drop it)", request.Dir(r.stateRoot, name))
		}
		return a.fail(exitFailed, "run request: %v", err)
	}
	if r.identity != nil {
		ids, err := r.kcp.Identities()
		if err != nil {
			r.writeSummaryQuiet()
			r.stop(context.WithoutCancel(ctx), false)
			return a.fail(exitFailed, "run request: the identity: %v", err)
		}
		ictx, cancel := context.WithTimeout(context.Background(), requestSummaryRead)
		obj, err := ids.GetMarketIdentity(ictx, marketrequester.RequesterIdentity)
		cancel()
		if err != nil {
			r.writeSummaryQuiet()
			r.stop(context.WithoutCancel(ctx), false)
			return a.fail(exitFailed, "run request: the identity: %v", err)
		}
		r.identityObject = obj
		expected := r.expectedPDSURL
		live := strings.TrimSpace(obj.Status.PdsURL)
		if expected != "" && live != "" && !compose.SamePDSURL(expected, live) {
			r.writeSummaryQuiet()
			r.stop(context.WithoutCancel(ctx), false)
			return a.fail(exitFailed, "run request: the identity PDS public URL is %s, not the recorded %s; refusing to post against a changed DID-document endpoint", live, expected)
		}
		if err := r.finalizeIdentity(); err != nil {
			r.writeSummaryQuiet()
			r.stop(context.WithoutCancel(ctx), false)
			return a.fail(exitFailed, "run request: the identity: %v", err)
		}
		r.identity.Close()
	}
	if bundle != nil {
		ref, err := r.kcp.PutSecrets(ctx, bundle, r.networkTimeout)
		if err != nil {
			r.stop(context.WithoutCancel(ctx), false)
			return a.fail(exitFailed, "run request: %v", err)
		}
		r.contract.SecretsRef = &ref
		r.log("the secrets bundle is in the invocation's own OpenBao", "mount", ref.Mount, "path", ref.Path)
	}
	if err := r.kcp.CreateContract(ctx, r.contract, r.networkTimeout); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		return a.fail(exitFailed, "run request: %v", err)
	}
	return r.carryOn(ctx)
}

func (r *requestRun) writeSummaryQuiet() {
	if err := request.WriteSummary(r.stateRoot, r.summary); err != nil {
		r.a.log.Warn("writing summary.json", "err", err.Error())
	}
}

func (a *app) loadRequest(vals clicfg.Values, rest []string, verb string) (*requestRun, int) {
	pos, pass := splitWords(rest)
	if len(pos) != 1 || len(pass) > 0 {
		return nil, a.fail(exitRefused, "run request %s <name>: one invocation name, no request-vm-ssh options", verb)
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
	}
	inv, err := request.ReadInvocation(r.stateRoot, pos[0])
	if err != nil {
		return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
	}
	if inv.StateRoot != "" && inv.StateRoot != r.stateRoot {
		return nil, a.fail(exitRefused, "run request %s: invocation %s belongs to state root %s, not %s", verb, inv.Name, inv.StateRoot, r.stateRoot)
	}
	r.inv = inv
	if s, err := request.ReadSummary(r.stateRoot, inv.Name); err == nil {
		r.summary = s
	} else {
		r.summary = request.Summary{Name: inv.Name, Mode: inv.Mode, Network: inv.Network, Instance: inv.Instance, CreatedAt: inv.CreatedAt}
	}
	if r.summary.Identity == "" {
		r.summary.Identity = inv.Identity
	}
	if inv.Identity != "" {
		instanceState := request.InstanceStateDir(r.stateRoot, inv.Name)
		if _, err := os.Stat(instanceState); err != nil {
			if verb != "release" {
				return nil, a.fail(exitRefused, "run request %s: invocation %s's kcp state is gone; start a new invocation with --identity %s", verb, inv.Name, inv.Identity)
			}
			r.cleanupOnly = true
			return r, exitOK
		}
		if err := request.LinkIdentityExposure(r.stateRoot, inv.Identity, instanceState); err != nil {
			return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
		}
	}
	return r, exitOK
}

func (a *app) reopenRequest(ctx context.Context, vals clicfg.Values, rest []string, verb string) (*requestRun, int) {
	r, code := a.loadRequest(vals, rest, verb)
	if r == nil {
		return nil, code
	}
	if r.cleanupOnly {
		return r, exitOK
	}
	r.log("starting the invocation's kcp again from its state dir", "verb", verb)
	r.resuming = verb == "resume"
	if err := r.start(ctx, false); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		return nil, a.fail(exitFailed, "run request %s: %v", verb, err)
	}
	return r, exitOK
}

func runRequestResume(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.reopenRequest(ctx, vals, rest, "resume")
	if r == nil {
		return code
	}
	return r.carryOn(ctx)
}

func runRequestRelease(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.reopenRequest(ctx, vals, rest, "release")
	if r == nil {
		return code
	}
	if r.cleanupOnly {
		r.stop(context.WithoutCancel(ctx), true)
		if err := os.RemoveAll(request.Dir(r.stateRoot, r.inv.Name)); err != nil {
			return a.fail(exitFailed, "run request release: removing the invocation: %v", err)
		}
		r.log("the invocation's kcp state was already gone; the invocation is removed and the identity store is kept")
		r.a.printJSON(r.summary)
		return exitOK
	}
	c, err := r.release(ctx)
	if err != nil || c == nil || c.Status.Phase != v1alpha1.ComputeContractReleased {
		phase := ""
		if c != nil {
			phase = string(c.Status.Phase)
		}
		r.a.log.Error("the release did not finish", "err", fmt.Sprint(err), "phase", phase)
		r.writeSummary(ctx, c)
		r.stop(context.WithoutCancel(ctx), false)
		r.writeSummary(ctx, nil)
		r.a.printJSON(r.summary)
		return exitFailed
	}
	r.writeSummary(ctx, c)
	r.stop(context.WithoutCancel(ctx), true)
	if r.standalone.StateDir != "" {
		if _, err := os.Stat(r.standalone.StateDir); err == nil {
			r.writeSummary(ctx, nil)
			r.a.printJSON(r.summary)
			return a.fail(exitFailed, "run request release: the invocation's processes remain; its state is kept")
		}
	}
	r.writeSummary(ctx, nil)
	r.a.printJSON(r.summary)
	return exitOK
}

func runRequestList(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return a.fail(exitRefused, "run request list: %v", err)
	}
	if root, err = filepath.Abs(root); err != nil {
		return a.fail(exitRefused, "run request list: %v", err)
	}
	list, err := request.List(root)
	if err != nil {
		return a.fail(exitFailed, "run request list: %v", err)
	}
	if vals.Bool("json") {
		a.printJSON(list)
		return exitOK
	}
	for _, s := range list {
		c := request.ContractSummary{}
		if s.Contract != nil {
			c = *s.Contract
		}
		where := s.Mode
		if s.Network != "" {
			where = "network " + s.Network
		}
		fmt.Fprintf(a.stdout, "%-24s %-20s %-10s kcp=%-5t identity=%s did=%s winner=%s guest=%s %s\n", s.Name, where, cmp.Or(s.Phase), s.KcpRunning,
			cmp.Or(s.Identity, "-"), cmp.Or(s.Did, "-"), cmp.Or(c.WinnerDid), cmp.Or(c.VMAddress), s.Message)
	}
	if len(list) == 0 {
		fmt.Fprintln(a.stdout, "no invocations under", request.Root(root))
	}
	return exitOK
}

func (r *requestRun) keepLogs() {
	if r.instanceDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(r.instanceDir, "logs", "core.log"))
	if err != nil {
		return
	}
	out := filepath.Join(request.Dir(r.stateRoot, r.inv.Name), "logs")
	if err := os.MkdirAll(out, 0o700); err != nil {
		return
	}
	os.WriteFile(filepath.Join(out, "core.log"), b, 0o600)
}

func (r *requestRun) attach(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestAttachProbe)
	defer cancel()
	var kubeconfig, workspace string
	switch r.inv.Mode {
	case request.ModeNetwork:
		netLayout, netRec, err := r.networkLayout()
		if err != nil {
			return err
		}
		app, ok, err := request.NetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name)
		if err != nil {
			return err
		}
		if !ok || app.Instance == "" || app.Message == marketnetwork.StoppedMessage {
			return errors.New("the market network does not run the invocation")
		}
		rec, err := r.childRecord(netLayout, app.Instance)
		if err != nil {
			return err
		}
		r.instanceDir = rec.Layout.StateDir
		kubeconfig, workspace = rec.AdminKubeconfig, rec.BaseWorkspace
	default:
		l, err := r.a.layoutOf(r.inv.Name, r.vals.With("instance", r.inv.Name).With("state-dir", request.Dir(r.stateRoot, r.inv.Name)))
		if err != nil {
			return err
		}
		var rec devinstance.Record
		if err := fsx.ReadJSON(l.Record, &rec); err != nil {
			return err
		}
		if rec.AdminKubeconfig == "" {
			return errors.New("the invocation's kcp has no admin kubeconfig")
		}
		r.standalone, r.instanceDir = l, l.StateDir
		kubeconfig = rec.AdminKubeconfig
	}
	k, err := request.OpenKcp(kubeconfig, workspace)
	if err != nil {
		return err
	}
	st, err := k.Root(ctx)
	if err != nil {
		return err
	}
	if st.Phase != v1alpha1.ApplicationPhaseReady {
		return fmt.Errorf("the MarketRequester is %s", st.Phase)
	}
	r.kcp = k.ForContract(r.target)
	return nil
}

func (a *app) openRequest(ctx context.Context, vals clicfg.Values, rest []string, verb string) (*requestRun, bool, int) {
	r, code := a.loadRequest(vals, rest, verb)
	if r == nil {
		return nil, false, code
	}
	err := r.attach(ctx)
	if err == nil {
		return r, false, exitOK
	}
	r.log("the invocation's kcp does not answer; starting it again from its state dir", "verb", verb, "why", err.Error())
	if err := r.start(ctx, false); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		return nil, false, a.fail(exitFailed, "run request %s: %v", verb, err)
	}
	return r, true, exitOK
}

func runRequestSSH(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, started, code := a.openRequest(ctx, vals, rest, "ssh")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request ssh: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request ssh: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	ec, err := r.ssh(ctx, c)
	if err != nil {
		return a.fail(exitFailed, "run request ssh: %v", err)
	}
	if ec != 0 {
		return exitFailed
	}
	return exitOK
}

func runRequestProxy(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	level, err := logx.ParseLevel(vals.String("log-level"))
	if err != nil {
		return a.fail(exitRefused, "run request proxy: --log-level: %v", err)
	}
	quiet := *a
	quiet.stdout = a.stderr
	handler := &slog.HandlerOptions{Level: level}
	if level >= slog.LevelWarn {
		handler.ReplaceAttr = dropTime
	}
	quiet.log = slog.New(slog.NewTextHandler(a.stderr, handler)).With("service", serviceName)
	r, started, code := quiet.openRequest(ctx, vals, rest, "proxy")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	if r.tag == "" {
		return a.fail(exitRefused, "run request proxy: --tag names the webrtc tunnel")
	}
	log := quiet.log.With("request", r.inv.Name)
	conn, timings, err := webrtcproxy.Dial(ctx, r.kcp.Signaling(), webrtcproxy.Options{Tag: r.tag, Log: log})
	if err != nil {
		return a.fail(exitFailed, "run request proxy: %s", strings.Join(strings.Fields(err.Error()), " "))
	}
	defer conn.Close()
	log.Info("the proxy pipes stdin and stdout through the datachannel", "tag", r.tag, "guestSeconds", timings.Guest.Seconds(),
		"offerToAnswerSeconds", timings.Answer.Seconds(), "offerToOpenSeconds", timings.Open.Seconds())
	if err := webrtcproxy.Pipe(ctx, conn, os.Stdin, a.stdout); err != nil && ctx.Err() == nil {
		log.Warn("the proxy ends", "err", err.Error())
	}
	return exitOK
}

func dropTime(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 && attr.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return attr
}

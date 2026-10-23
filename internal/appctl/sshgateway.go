package appctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	SshGatewaySet            = "ssh-gateway"
	SshGatewayApp            = "ssh-gateway"
	SshGatewayComponent      = "ssh-gateway"
	SshGatewayVaultServer    = "ssh-gateway"
	SshGatewayVaultClient    = "ssh-gateway"
	SshGatewayAccount        = "ssh-gateway"
	SshGatewayIrohDial       = "iroh-dial"
	SshGatewayIrohSocket     = "iroh-dial.sock"
	SshGatewayWebDir         = "ts/socialweb-computer-ssh/web"
	SshGatewayProfileNetwork = "market-network"
	SshGatewayProfileDefault = "default"
	DumbpipeExecutable       = "dumbpipe"
)

var (
	SshGatewayGVR      = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "sshgateways"}
	SshGatewayProfiles = []string{SshGatewayProfileNetwork, SshGatewayProfileDefault}
	SshGatewayEnvAllow = []string{"KCP_SERVICE_DOMAIN", "KCP_DNS_TABLE", "KCP_DNS_TABLE_FILE", "KCP_TOKENS", "KCP_SERVER",
		"KCP_WORKSPACE", "KCP_NAMESPACE", "KCP_SERVICE_NAME", "PORT_FILE", "LISTENERS_FILE", "STATUS_FILE", "MIN_LOG_LEVEL", "ATPROTO_DID", "BUNDLED_ACTIONS_DIR", "POLICY_ENGINE_LOG_LEVEL", "PATH"}
)

func SshGatewayValues(spec v1alpha1.SshGatewaySpec, stateDir string, l devinstance.Layout, rec devinstance.Record) (manifests.SshGateway, error) {
	v := manifests.SshGateway{StateDir: stateDir}
	if !slices.Contains(SshGatewayProfiles, spec.Profile) {
		return v, fmt.Errorf("appctl: SshGateway profile %q: want one of %s", spec.Profile, strings.Join(SshGatewayProfiles, ", "))
	}
	v.WebDir = manifests.UnderTSRoot(SshGatewayWebDir)
	dumbpipe := rec.Executables[DumbpipeExecutable].Path
	if dumbpipe == "" {
		return v, errors.New("appctl: the instance has no dumbpipe executable for the iroh dialer (dev up --dumbpipe-path in a dev build)")
	}
	v.IrohDial = manifests.IrohDial{Unit: l.Names().HostProcessPrefix() + SshGatewayIrohDial + ".service",
		Socket: filepath.Join(stateDir, SshGatewayIrohSocket), Dumbpipe: dumbpipe}
	if err := (irohdial.Config{Socket: v.IrohDial.Socket, Dumbpipe: dumbpipe}).Check(); err != nil {
		return v, err
	}
	env, err := json.Marshal(SshGatewayEnvAllow)
	if err != nil {
		return v, err
	}
	v.EnvAllow = string(env)
	var extra, read []string
	if n := spec.Network; n != nil {
		v.CABundle = n.CaBundleRef
		for _, kv := range [][2]string{
			{"--plc-directory-url", n.PlcDirectoryUrl},
			{"--ingress-proxy-host", n.IngressProxyHost},
			{"--relay-url", strings.Join(n.RelayUrls, ",")},
			{"--network-hosts", NetworkHostsArg(n.Hosts)},
		} {
			if kv[1] != "" {
				extra = append(extra, kv[0], kv[1])
			}
		}
		if spec.Profile == SshGatewayProfileNetwork {
			aliases, err := guestAliases(n)
			if err != nil {
				return v, err
			}
			if len(aliases) > 0 {
				extra = append(extra, "--guest-host-aliases", strings.Join(aliases, ","))
			}
		}
	}
	if spec.Profile == SshGatewayProfileNetwork && (spec.Network == nil || v.CABundle == "") {
		return v, errors.New("appctl: the market-network profile needs the network value with its caBundleRef; the MarketNetwork composer copies it")
	}
	if len(spec.BidderDids) > 0 {
		extra = append(extra, "--bidder-dids", strings.Join(spec.BidderDids, ","))
	}
	if f := spec.OwnerSessionsFile; f != "" {
		if spec.Profile != SshGatewayProfileNetwork {
			return v, fmt.Errorf("appctl: ownerSessionsFile is a value of the %s profile only, not of %s", SshGatewayProfileNetwork, spec.Profile)
		}
		if !filepath.IsAbs(f) {
			return v, fmt.Errorf("appctl: ownerSessionsFile %q is not an absolute path", f)
		}
		extra, read = append(extra, "--test-owner-sessions-file", f), append(read, f)
	}
	opts, err := OptionArgs(SshGatewayApp, spec.Options)
	if err != nil {
		return v, err
	}
	extra = append(extra, opts...)
	if v.ExtraArgs, err = fragment(extra); err != nil {
		return v, err
	}
	if v.ExtraRead, err = fragment(read); err != nil {
		return v, err
	}
	if v.VaultExtra, err = SshGatewayVaultExtra(spec); err != nil {
		return v, err
	}
	return v, nil
}

func SshGatewayVaultExtra(spec v1alpha1.SshGatewaySpec) (string, error) {
	plugin, err := AtprotoPlugin(spec.Options, spec.Network, SshGatewayAtprotoScope())
	if err != nil {
		return "", err
	}
	raw, err := yaml.Marshal(map[string]any{
		"plugins":  []v1alpha1.OpenBaoPlugin{plugin},
		"declared": v1alpha1.OpenBaoServerDeclared{Policies: AtprotoPolicies()},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String(), nil
}

func (ctl *Controller) sshGatewayKcp(ctx context.Context, stateDir string) (manifests.KcpRef, manifests.BidderVault, error) {
	if _, _, _, err := ctl.vaultRefs(ctx, SshGatewayVaultServer, SshGatewayVaultClient); err != nil {
		return manifests.KcpRef{}, manifests.BidderVault{}, err
	}
	vault := manifests.BidderVault{Server: SshGatewayVaultServer, Client: SshGatewayVaultClient}
	return manifests.KcpRef{Workspace: ctl.c.workspace()}, vault, nil
}

func (ctl *Controller) ownedHostProcesses(ctx context.Context, owner string) ([]unstructured.Unstructured, error) {
	ri, err := ctl.client(ctl.c.workspace(), HostProcessGVR)
	if err != nil {
		return nil, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (ctl *Controller) deleteHostProcesses(ctx context.Context, owner string) (int, error) {
	items, err := ctl.ownedHostProcesses(ctx, owner)
	if err != nil {
		return 0, err
	}
	ri, err := ctl.client(ctl.c.workspace(), HostProcessGVR)
	if err != nil {
		return 0, err
	}
	for _, o := range items {
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		if err := ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return len(items), err
		}
	}
	return len(items), nil
}

func ownerIdentityOf(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var sessions map[string]ownerSession
	if json.Unmarshal(b, &sessions) != nil || len(sessions) == 0 {
		return "", ""
	}
	dids := make([]string, 0, len(sessions))
	for did := range sessions {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	return dids[0], sessions[dids[0]].Identifier
}

func sshGatewayIdentityHandle(o v1alpha1.SshGateway) string {
	if handle := strings.TrimSpace(o.Spec.Identity.Handle); handle != "" {
		return handle
	}
	return optionValue(o.Spec.Options, "app-handle")
}

func sshGatewayAccountIdentity(o v1alpha1.SshGateway) v1alpha1.ApplicationIdentity {
	id := o.Spec.Identity
	if id.Mode == "" {
		id.Mode = v1alpha1.MarketIdentityMode(optionValue(o.Spec.Options, "atproto-identity-mode"))
	}
	if id.Handle == "" {
		id.Handle = optionValue(o.Spec.Options, "app-handle")
	}
	if id.PdsUrl == "" {
		id.PdsUrl = optionValue(o.Spec.Options, "atproto-pds-url")
	}
	return id
}

func (ctl *Controller) sshRequesterIdentity(ctx context.Context, owner string, o v1alpha1.SshGateway, plan accountPlan) (*v1alpha1.ComponentIdentity, error) {
	handle := sshGatewayIdentityHandle(o)
	if did := optionValue(o.Spec.Options, "app-did"); did != "" {
		return &v1alpha1.ComponentIdentity{Did: did, Handle: handle}, nil
	}
	if did, h := ownerIdentityOf(o.Spec.OwnerSessionsFile); did != "" {
		if h != "" {
			handle = h
		}
		return &v1alpha1.ComponentIdentity{Did: did, Handle: handle}, nil
	}
	if err := ctl.ensureIdentitySpec(ctx, ctl.c.workspace(), owner, RequesterIdentity, plan.Identity); err != nil {
		return nil, err
	}
	id, err := ctl.identityIn(ctx, ctl.c.workspace(), RequesterIdentity)
	if apierrors.IsNotFound(err) {
		return &v1alpha1.ComponentIdentity{MarketIdentityRef: ComponentRefFor("MarketIdentity", RequesterIdentity), Handle: handle}, nil
	}
	if err != nil {
		return nil, err
	}
	return componentIdentity(id, handle), nil
}

func sshGatewayIrohSocket(stateRoot string) string {
	return filepath.Join(ComponentCRStateDir(stateRoot, ComponentKindSshServer, SshServerComponent), SshGatewayIrohSocket)
}

func sshServerExposuresReady(snap componentSnapshot, desired []desiredComponent) bool {
	prefix := serviceNameFor(ComponentKindSshServer, SshServerComponent)
	found := false
	for _, d := range desired {
		if d.Kind != ComponentKindExposure || !strings.HasPrefix(d.Name, prefix) {
			continue
		}
		found = true
		if !snap.ready(d.Kind, d.Name) {
			return false
		}
	}
	return found
}

func sshGatewayDesired(o v1alpha1.SshGateway, plan accountPlan, ident *v1alpha1.ComponentIdentity, ws, stateRoot string) []desiredComponent {
	suspend := o.Spec.Suspend
	network := o.Spec.Network
	vault := ComponentRefFor("OpenBaoServer", SshGatewayVaultServer)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	mode, publicOrigin, oauthClientID, oauthRedirectURI, guestSSHKeysFile := requesterComponentFields(o.Spec.Options, plan.Mode)
	var requesterPdsRef *v1alpha1.ComponentRef
	if plan.Local {
		requesterPdsRef = nil
	} else {
		requesterPdsRef = plan.PdsRef
	}
	requester := desiredComponent{Kind: ComponentKindRequester, Name: RequesterComponent, Spec: v1alpha1.RequesterSpec{
		ComponentSpec:      NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindRequester, RequesterComponent), network, suspend),
		Identity:           ident,
		PdsRef:             requesterPdsRef,
		PDSAccessTokenPath: plan.accessTokenPath(),
		PolicyEngineRef:    ComponentRefFor(ComponentKindPolicyEngine, PolicyEngineComponent),
		Hostnames:          hostnames,
		Mode:               mode,
		PublicOrigin:       publicOrigin,
		OAuthClientID:      oauthClientID,
		OAuthRedirectURI:   oauthRedirectURI,
		GuestSSHKeysFile:   guestSSHKeysFile,
		OwnerSessionsFile:  o.Spec.OwnerSessionsFile,
		IrohSocket:         sshGatewayIrohSocket(stateRoot),
		AcceptCopies:       int32(optionInt(o.Spec.Options, "accept-copies")),
		Comms:              o.Spec.Comms,
	}}
	sshServer := desiredComponent{Kind: ComponentKindSshServer, Name: SshServerComponent, Spec: v1alpha1.SshServerSpec{
		ComponentSpec:     NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindSshServer, SshServerComponent), network, suspend),
		Identity:          ident,
		RequesterRef:      ComponentRefFor(ComponentKindRequester, RequesterComponent),
		VaultRef:          vault,
		Hostnames:         hostnames,
		OwnerSessionsFile: o.Spec.OwnerSessionsFile,
	}}
	out := []desiredComponent{policyEngineDesired(suspend), requester, sshServer}
	out = append(out, httpServiceExposures(ws, ComponentKindRequester, RequesterComponent, "http", network, hostnames, false, true, suspend)...)
	out = append(out, httpServiceExposures(ws, ComponentKindSshServer, SshServerComponent, "http", network, hostnames, true, false, suspend)...)
	out = append(out, sshExposures(ws, SshServerComponent, suspend)...)
	return out
}

func (ctl *Controller) reconcileSshGateway(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.SshGateway
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile}
	if u.GetDeletionTimestamp() != nil {
		left, err := ctl.stopComponents(ctx, owner)
		if err != nil {
			return err
		}
		if left == 0 {
			if left, err = ctl.deleteHostProcesses(ctx, owner); err != nil {
				return err
			}
		}
		if left == 0 {
			if left, err = ctl.deleteVaults(ctx, owner); err != nil {
				return err
			}
		}
		if left > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d component(s) stopping", left)
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		delete(ctl.applied, string(o.UID))
		return ctl.setFinalizer(ctx, SshGatewayGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := ctl.setFinalizer(ctx, SshGatewayGVR, u, true); err != nil {
			return err
		}
	}
	stateRoot := ctl.c.Layout.StateDir
	plan, err := ctl.planAccount(ctx, ctl.c.workspace(), sshGatewayAccountIdentity(o), "requester",
		accountVault{Server: SshGatewayVaultServer, Client: SshGatewayVaultClient, Mount: SshGatewayVaultServer},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
	}
	if o.Spec.Suspend {
		ident, err := ctl.sshRequesterIdentity(ctx, owner, o, plan)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the ssh requester's identity: "+err.Error()
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		desired := sshGatewayDesired(o, plan, ident, ctl.c.workspace(), stateRoot)
		if err := ctl.ensureDesired(ctx, u, desired); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		_ = ctl.pruneDesired(ctx, owner, desired)
		snap, err := ctl.componentSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		st.Components, st.Listeners = snap.Components, snap.Listeners
		st.Did = ident.Did
		st.Phase, st.Message = v1alpha1.ApplicationPhasePending, "suspended"
		return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
	}
	if ctl.applied[string(o.UID)] != o.Generation {
		rec, err := ctl.c.Record()
		if err != nil {
			return err
		}
		stateDir := ComponentCRStateDir(stateRoot, ComponentKindSshServer, SshServerComponent)
		values, err := SshGatewayValues(o.Spec, stateDir, ctl.c.Layout, rec)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "applying its own OpenBaoServer, its iroh dialer and its workspace"
		ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		if err := ctl.applyInfra(ctx, SshGatewaySet, o.Spec.Profile, owner, &values); err != nil {
			st.Message = err.Error()
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		if _, _, err := ctl.sshGatewayKcp(ctx, stateDir); err != nil {
			st.Message = "its vault and its kcp account: " + err.Error()
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		if err := ctl.seedPdsAccountPassword(ctx, plan); err != nil {
			st.Message = "the ssh requester's account password: " + err.Error()
			return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
		}
		ctl.applied[string(o.UID)] = o.Generation
	}
	ident, err := ctl.sshRequesterIdentity(ctx, owner, o, plan)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the ssh requester's identity: "+err.Error()
		return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
	}
	desired := sshGatewayDesired(o, plan, ident, ctl.c.workspace(), stateRoot)
	if err := ctl.ensureDesired(ctx, u, desired); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
	}
	if err := ctl.ensureMarketStreams(ctx, ctl.c.workspace(), "requester", o.Spec.Comms, o.Spec.Network, oauthReceiveOnly(plan.Mode)); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
	}
	if err := ctl.pruneDesired(ctx, owner, desired); err != nil {
		return err
	}
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	st.Components, st.Listeners = snap.Components, snap.Listeners
	st.Did = ident.Did
	hps, err := ctl.ownedHostProcesses(ctx, owner)
	if err != nil {
		return err
	}
	dialer := false
	for _, h := range hps {
		phase, _, _ := unstructured.NestedString(h.Object, "status", "phase")
		running := phase == string(v1alpha1.HostProcessPhaseRunning)
		st.Components = append(st.Components, v1alpha1.ApplicationComponent{Kind: "HostProcess", Name: h.GetName(),
			Workspace: ctl.c.workspace(), Phase: phase, Ready: running})
		dialer = dialer || (h.GetName() == SshGatewayIrohDial && running)
	}
	switch {
	case snap.complete(desired) && sshServerExposuresReady(snap, desired) && dialer && ident.Did != "":
		st = ready(st, "the ssh gateway listens, its iroh dialer runs and its owner identity is known")
	case ident.Did == "":
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the owner's atproto identity"
	case !dialer:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the iroh dialer HostProcess to run"
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for every component to be ready"
	}
	return ctl.writeStatus(ctx, SshGatewayGVR, o.Name, st)
}

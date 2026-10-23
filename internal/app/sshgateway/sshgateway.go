package sshgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
)

const (
	Name = "sshgateway"

	Kind = string(v1alpha1.ApplicationKindSshGateway)

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
	SshServerComponent       = "ssh-server"

	guestRouteProbe = "1.1.1.1:53"
)

var (
	SshGatewayProfiles = []string{SshGatewayProfileNetwork, SshGatewayProfileDefault}
	SshGatewayEnvAllow = []string{"KCP_SERVICE_DOMAIN", "KCP_DNS_TABLE", "KCP_DNS_TABLE_FILE", "KCP_TOKENS", "KCP_SERVER",
		"KCP_WORKSPACE", "KCP_NAMESPACE", "KCP_SERVICE_NAME", "PORT_FILE", "LISTENERS_FILE", "STATUS_FILE", "MIN_LOG_LEVEL", "ATPROTO_DID", "BUNDLED_ACTIONS_DIR", "POLICY_ENGINE_LOG_LEVEL", "PATH"}
)

func GuestHostIP() (string, error) {
	conn, err := net.Dial("udp4", guestRouteProbe)
	if err != nil {
		return "", fmt.Errorf("sshgateway: the host address guests reach: %w", err)
	}
	defer conn.Close()
	a, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || a.IP.IsLoopback() || a.IP.IsUnspecified() {
		return "", fmt.Errorf("sshgateway: the host has no routable address for guests (%v)", conn.LocalAddr())
	}
	return a.IP.String(), nil
}

func guestAliases(n *v1alpha1.MarketNetworkValue) ([]string, error) {
	var names []string
	for _, h := range []string{compose.HostOf(n.IngressProxyHost), compose.HostOf(n.GuestTLSURL())} {
		if h != "" && net.ParseIP(h) == nil && !slices.Contains(names, h) {
			names = append(names, h)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	ip, err := GuestHostIP()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, h := range names {
		out = append(out, ip+" "+h)
	}
	return out, nil
}

func SshGatewayValues(spec v1alpha1.SshGatewaySpec, stateDir string, l devinstance.Layout, rec devinstance.Record) (manifests.SshGateway, error) {
	v := manifests.SshGateway{StateDir: stateDir}
	if !slices.Contains(SshGatewayProfiles, spec.Profile) {
		return v, fmt.Errorf("sshgateway: SshGateway profile %q: want one of %s", spec.Profile, strings.Join(SshGatewayProfiles, ", "))
	}
	v.WebDir = manifests.UnderTSRoot(SshGatewayWebDir)
	dumbpipe := rec.Executables[DumbpipeExecutable].Path
	if dumbpipe == "" {
		return v, errors.New("sshgateway: the instance has no dumbpipe executable for the iroh dialer (dev up --dumbpipe-path in a dev build)")
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
		caFile, err := compose.NetworkCAFile(stateDir, n)
		if err != nil {
			return v, err
		}
		v.CABundle = caFile
		for _, kv := range [][2]string{
			{"--plc-directory-url", n.PLCDirectoryURL},
			{"--ingress-proxy-host", n.IngressProxyHost},
			{"--network-hosts", compose.NetworkHostsArg(n.Hosts)},
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
		return v, errors.New("sshgateway: the market-network profile needs the network value with its caBundleRef; the MarketNetwork composer copies it")
	}
	if len(spec.BidderDids) > 0 {
		extra = append(extra, "--bidder-dids", strings.Join(spec.BidderDids, ","))
	}
	if f := spec.OwnerSessionsFile; f != "" {
		if spec.Profile != SshGatewayProfileNetwork {
			return v, fmt.Errorf("sshgateway: ownerSessionsFile is a value of the %s profile only, not of %s", SshGatewayProfileNetwork, spec.Profile)
		}
		if !filepath.IsAbs(f) {
			return v, fmt.Errorf("sshgateway: ownerSessionsFile %q is not an absolute path", f)
		}
		extra, read = append(extra, "--test-owner-sessions-file", f), append(read, f)
	}
	opts, err := compose.OptionArgs(SshGatewayApp, spec.Options)
	if err != nil {
		return v, err
	}
	extra = append(extra, opts...)
	if v.ExtraArgs, err = compose.Fragment(extra); err != nil {
		return v, err
	}
	if v.ExtraRead, err = compose.Fragment(read); err != nil {
		return v, err
	}
	if v.VaultExtra, err = SshGatewayVaultExtra(spec); err != nil {
		return v, err
	}
	return v, nil
}

func SshGatewayVaultExtra(spec v1alpha1.SshGatewaySpec) (string, error) {
	plugin, err := compose.AtprotoPlugin(spec.Options, spec.Network, compose.SshGatewayAtprotoScope())
	if err != nil {
		return "", err
	}
	raw, err := yaml.Marshal(map[string]any{
		"plugins":  []v1alpha1.OpenBaoPlugin{plugin},
		"declared": v1alpha1.OpenBaoServerDeclared{Policies: compose.AtprotoPolicies()},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String(), nil
}

type Controller struct {
	c *compose.Composer
}

func Register(m *ctlruntime.Manager, c *compose.Composer) error {
	ctl := &Controller{c: c}
	opts := append([]ctlruntime.Option{ctlruntime.Export(compose.ExportApplications), ctlruntime.Finalizer(compose.Finalizer),
		childHostProcesses()}, c.Watches(Kind)...)
	if err := ctlruntime.For[*v1alpha1.SshGateway](m, Name, ctl, opts...); err != nil {
		return fmt.Errorf("sshgateway: registering the SshGateway controller: %w", err)
	}
	return nil
}

func childHostProcesses() ctlruntime.Option {
	return ctlruntime.WatchesIn(compose.ExportHostProcesses, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.HostProcess) []ctlruntime.Request {
		labels := obj.GetLabels()
		name, ok := strings.CutPrefix(labels[compose.OwnerLabel], strings.ToLower(Kind)+".")
		if !ok || name == "" {
			return nil
		}
		cluster := src.Cluster
		if lc := strings.TrimSpace(labels[compose.RootClusterLabel]); lc != "" {
			cluster = lc
		}
		return []ctlruntime.Request{{Cluster: cluster, Name: name}}
	})
}

type pass struct {
	ctx context.Context

	c *compose.Composer

	r compose.Root

	o *v1alpha1.SshGateway

	st v1alpha1.ApplicationStatus
}

func (p *pass) write(phase v1alpha1.ApplicationPhase, msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = phase, msg
	return ctlruntime.Result{}, p.c.WriteStatus(p.ctx, p.r, compose.SshGatewayGVR, p.st)
}

func (p *pass) fail(err error) (ctlruntime.Result, error) {
	compose.SetOverrideErrorIfAny(&p.st.Conditions, &p.st.ComponentSummary, p.o.Generation, err)
	return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
}

func (p *pass) retry(what string, err error) (ctlruntime.Result, error) {
	msg := err.Error()
	if what != "" {
		msg = what + ": " + msg
	}
	if _, werr := p.write(v1alpha1.ApplicationPhaseProvisioning, msg); werr != nil {
		return ctlruntime.Result{}, errors.Join(err, werr)
	}
	return ctlruntime.Result{}, err
}

func (p *pass) wait(msg string) (ctlruntime.Result, error) {
	return p.write(v1alpha1.ApplicationPhaseProvisioning, msg)
}

func (ctl *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.SshGateway) (ctlruntime.Result, error) {
	c := ctl.c
	if !c.Mine(req) {
		return ctlruntime.Result{}, nil
	}
	r := compose.RootOf(req, Kind, o, o.Spec.Suspend)
	c.Observe(r)
	p := &pass{ctx: ctx, c: c, r: r, o: o, st: compose.NewStatus(o.Generation, o.Spec.Profile, o.Status.Conditions)}
	if r.Deleting {
		return p.delete(ctx)
	}
	res, err := p.reconcile(ctx)
	if err == nil {
		c.Adopt(ctx, r)
		c.ApplyQuotas(ctx, r, o.Spec.Quota)
	}
	return res, err
}

func (p *pass) delete(ctx context.Context) (ctlruntime.Result, error) {
	left, err := p.c.StopComponents(ctx, p.r, "")
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if left == 0 {
		if left, err = p.c.DeleteOwned(ctx, p.r.Workspace, compose.HostProcessGVR, p.r.Owner()); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if left == 0 {
		if left, err = p.c.DeleteVaults(ctx, p.r); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if left > 0 {
		p.st.Phase, p.st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d component(s) stopping", left)
		return ctlruntime.Result{Waiting: true}, p.c.WriteStatus(ctx, p.r, compose.SshGatewayGVR, p.st)
	}
	p.c.Drop(p.r)
	return ctlruntime.Result{}, nil
}

func identityHandle(o *v1alpha1.SshGateway) string {
	if handle := strings.TrimSpace(o.Spec.Identity.Handle); handle != "" {
		return handle
	}
	return compose.OptionValue(o.Spec.Options, "app-handle")
}

func accountIdentity(o *v1alpha1.SshGateway) v1alpha1.ApplicationIdentity {
	id := o.Spec.Identity
	if id.Mode == "" {
		id.Mode = v1alpha1.MarketIdentityMode(compose.OptionValue(o.Spec.Options, "atproto-identity-mode"))
	}
	if id.Handle == "" {
		id.Handle = compose.OptionValue(o.Spec.Options, "app-handle")
	}
	if id.PdsURL == "" {
		id.PdsURL = compose.OptionValue(o.Spec.Options, "atproto-pds-url")
	}
	return id
}

func ownerIdentityOf(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	did, s, err := compose.FirstOwnerSession(path)
	if err != nil {
		return "", ""
	}
	return did, s.Identifier
}

func irohSocket(stateRoot string) string {
	return filepath.Join(compose.ComponentCRStateDir(stateRoot, compose.ComponentKindSshServer, SshServerComponent), SshGatewayIrohSocket)
}

func (p *pass) requesterIdentity(ctx context.Context, plan compose.AccountPlan, apply bool) (*v1alpha1.ComponentIdentity, *v1alpha1.MarketIdentity, error) {
	c, r, o := p.c, p.r, p.o
	handle := identityHandle(o)
	if did := compose.OptionValue(o.Spec.Options, "app-did"); did != "" {
		return &v1alpha1.ComponentIdentity{Did: did, Handle: handle}, nil, nil
	}
	if did, h := ownerIdentityOf(o.Spec.OwnerSessionsFile); did != "" {
		if h != "" {
			handle = h
		}
		return &v1alpha1.ComponentIdentity{Did: did, Handle: handle}, nil, nil
	}
	if apply {
		if err := c.ApplyIdentitySpec(ctx, r, r.Workspace, marketrequester.RequesterIdentity, plan.Identity); err != nil {
			return nil, nil, err
		}
	}
	id, err := c.Identity(ctx, r.Workspace, marketrequester.RequesterIdentity)
	if apierrors.IsNotFound(err) {
		return &v1alpha1.ComponentIdentity{MarketIdentityRef: compose.ComponentRefFor(compose.KindMarketIdentity, marketrequester.RequesterIdentity), Handle: handle}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return compose.ComponentIdentity(id, handle), id, nil
}

func (p *pass) desired(plan compose.AccountPlan, ident *v1alpha1.ComponentIdentity, id *v1alpha1.MarketIdentity) ([]compose.Desired, error) {
	o, ws := p.o, p.r.Workspace
	stateRoot := p.c.StateRoot()
	network := o.Spec.Network
	vault := compose.ComponentRefFor(compose.KindOpenBaoServer, SshGatewayVaultServer)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	mode, publicOrigin, oauthClientID, oauthRedirectURI, guestSSHKeysFile := compose.RequesterComponentFields(o.Spec.Options, plan.Mode)
	var requesterPdsRef *commonv1.ObjectRef
	if !plan.Local {
		requesterPdsRef = plan.PdsRef
	}
	requester := compose.Desired{Kind: compose.ComponentKindRequester, Name: marketrequester.RequesterComponent, Spec: v1alpha1.RequesterSpec{
		ComponentSpec:        compose.NewComponentSpec(compose.ComponentCRStateDir(stateRoot, compose.ComponentKindRequester, marketrequester.RequesterComponent), network),
		Identity:             ident,
		PdsRef:               requesterPdsRef,
		PdsDID:               plan.PdsDID,
		PDSAccessToken:       plan.AccessToken(),
		OperatorDID:          compose.OperatorDID(o.Spec.Identity.ApprovedAssociations, id),
		PolicyEngineRef:      compose.ComponentRefFor(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent),
		Hostnames:            hostnames,
		Mode:                 mode,
		PublicOrigin:         publicOrigin,
		OAuthClientID:        oauthClientID,
		OAuthRedirectURI:     oauthRedirectURI,
		GuestSSHKeysFile:     guestSSHKeysFile,
		OwnerSessionsFile:    o.Spec.OwnerSessionsFile,
		IrohSocket:           irohSocket(stateRoot),
		BidderDIDs:           append([]string(nil), o.Spec.BidderDids...),
		AcceptCopies:         int32(compose.OptionInt(o.Spec.Options, "accept-copies")),
		Comms:                o.Spec.Comms,
		SuspendParticipation: o.Spec.Suspend,
	}}
	sshServer := compose.Desired{Kind: compose.ComponentKindSshServer, Name: SshServerComponent, Spec: v1alpha1.SshServerSpec{
		ComponentSpec:     compose.NewComponentSpec(compose.ComponentCRStateDir(stateRoot, compose.ComponentKindSshServer, SshServerComponent), network),
		Identity:          ident,
		RequesterRef:      compose.ComponentRefFor(compose.ComponentKindRequester, marketrequester.RequesterComponent),
		VaultRef:          vault,
		Hostnames:         hostnames,
		OwnerSessionsFile: o.Spec.OwnerSessionsFile,
	}}
	out := []compose.Desired{compose.PolicyEngineDesired(), requester, sshServer}
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindRequester, marketrequester.RequesterComponent, "http", network, hostnames, false, true)...)
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindSshServer, SshServerComponent, "http", network, hostnames, true, false)...)
	out = append(out, compose.SSHExposures(ws, SshServerComponent)...)
	return compose.ApplyOverrides(o.Spec.Components, out)
}

func (p *pass) ensure(ctx context.Context, desired []compose.Desired) error {
	if err := p.c.ApplyLinks(ctx, p.r, "components", compose.ComponentLinks(p.r.Workspace, desired)); err != nil {
		return err
	}
	return p.c.EnsureComponents(ctx, p.r, desired)
}

func (p *pass) baseLinks(mount string) []compose.Link {
	ws := p.r.Workspace
	requester := compose.AppRef(compose.ComponentKindRequester, marketrequester.RequesterComponent, ws)
	server := compose.AppRef(compose.ComponentKindSshServer, SshServerComponent, ws)
	return []compose.Link{
		compose.PolicyRunLink(requester, compose.PolicyEngineRefIn(compose.PolicyEngineComponent, ws), marketrequester.Workflows...),
		compose.VaultLink(server, SshGatewayVaultServer, false, mount+"/*"),
		compose.RootVaultLink(p.r, SshGatewayVaultServer, mount+"/identity/*"),
	}
}

func (p *pass) snapshot(ctx context.Context) (compose.Snapshot, error) {
	snap, err := p.c.Snapshot(ctx, p.r.Workspace, p.r.Owner())
	if err != nil {
		return snap, err
	}
	p.st.Components, p.st.Listeners = snap.Components, snap.Listeners
	return snap, nil
}

func (p *pass) reconcile(ctx context.Context) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	stateRoot := c.StateRoot()
	plan, err := c.PlanAccount(ctx, ws, accountIdentity(o), "requester",
		compose.AccountVault{Server: SshGatewayVaultServer, Client: SshGatewayVaultClient, Mount: SshGatewayVaultServer},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		return p.fail(err)
	}
	mount := plan.Vault.MountOr(SshGatewayVaultServer)
	if r.Suspend {
		return p.suspended(ctx, plan)
	}
	if err := c.ApplyVault(ctx, r, SshGatewayVaultServer); err != nil {
		return ctlruntime.Result{}, err
	}
	links := p.baseLinks(mount)
	if err := c.ApplyLinks(ctx, r, "root", links); err != nil {
		return ctlruntime.Result{}, err
	}
	if !c.Applied(r) {
		record := c.Config().Record
		if record == nil {
			return p.fail(errors.New("sshgateway: the composer has no instance record"))
		}
		rec, err := record()
		if err != nil {
			return ctlruntime.Result{}, err
		}
		stateDir := compose.ComponentCRStateDir(stateRoot, compose.ComponentKindSshServer, SshServerComponent)
		values, err := SshGatewayValues(o.Spec, stateDir, c.Config().Layout, rec)
		if err != nil {
			return p.fail(err)
		}
		if _, err := p.wait("applying its own OpenBaoServer, its iroh dialer and its workspace"); err != nil {
			return ctlruntime.Result{}, err
		}
		if err := c.ApplyInfra(ctx, r, SshGatewaySet, o.Spec.Profile, &values); err != nil {
			return p.retry("", err)
		}
		c.MarkApplied(r)
	}
	if _, _, err := c.VaultRefs(ctx, ws, SshGatewayVaultServer, SshGatewayVaultClient); err != nil {
		return p.retry("its vault", err)
	}
	if ok, msg := c.LinksAccepted(ctx, r, links); !ok {
		return p.wait(msg)
	}
	if err := c.SeedPdsAccountPassword(ctx, r, plan); err != nil {
		return p.retry("the ssh requester's account password", err)
	}
	ident, id, err := p.requesterIdentity(ctx, plan, true)
	if err != nil {
		return p.retry("the ssh requester's identity", err)
	}
	desired, err := p.desired(plan, ident, id)
	if err != nil {
		return p.fail(err)
	}
	if err := p.ensure(ctx, desired); err != nil {
		return p.retry("", err)
	}
	streams, err := c.ApplyMarketStreams(ctx, r, compose.Streams{Role: "requester", Comms: o.Spec.Comms,
		Sources: compose.SourceInput{Network: o.Spec.Network}, ForceFirehose: compose.OAuthReceiveOnly(plan.Mode)})
	if err != nil {
		return p.fail(err)
	}
	if err := c.ApplyLinks(ctx, r, "events", streams); err != nil {
		return ctlruntime.Result{}, err
	}
	if _, err := c.DeleteComponentsExcept(ctx, ws, r.Owner(), compose.DesiredKeys(desired)); err != nil {
		return ctlruntime.Result{}, err
	}
	var repoLinks []compose.Link
	if ident.Did != "" {
		if id != nil && !plan.Local {
			accountLinks, err := c.AccountRepo(ctx, r, ws, marketrequester.RequesterIdentity, plan, id.Status.Did, id.Status.PdsURL)
			if err != nil {
				c.Log().Warn("the existing-account repo", "root", r.Owner(), "err", err.Error())
			}
			repoLinks = append(repoLinks, accountLinks...)
		}
		requester := compose.AppRef(compose.ComponentKindRequester, marketrequester.RequesterComponent, ws)
		repoLinks = append(repoLinks, compose.RepoWriteLink(requester, compose.RepoRefIn(ident.Did, ws), compose.ScopeAll))
	}
	if err := c.ApplyLinks(ctx, r, "repo", repoLinks); err != nil {
		return ctlruntime.Result{}, err
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = ident.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	dialer, err := p.hostProcesses(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	switch {
	case snap.Satisfied(desired) && exposuresReady(snap, desired) && dialer && ident.Did != "":
		p.st = compose.Ready(p.st, "the ssh gateway listens, its iroh dialer runs and its owner identity is known")
		return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.SshGatewayGVR, p.st)
	case ident.Did == "":
		return p.wait("waiting for the owner's atproto identity")
	case !dialer:
		return p.wait("waiting for the iroh dialer HostProcess to run")
	}
	return p.wait("waiting for every component to be ready")
}

func (p *pass) hostProcesses(ctx context.Context) (bool, error) {
	ri, err := p.c.Client(p.r.Workspace, compose.HostProcessGVR)
	if err != nil {
		return false, err
	}
	list, err := kube.List[v1alpha1.HostProcess](ctx, ri, metav1.ListOptions{LabelSelector: compose.OwnerLabel + "=" + p.r.Owner()})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dialer := false
	for _, h := range list {
		phase := string(h.Status.Phase)
		running := phase == string(v1alpha1.HostProcessPhaseRunning)
		p.st.Components = append(p.st.Components, v1alpha1.ApplicationComponent{Kind: compose.KindHostProcess, Name: h.Name,
			Workspace: p.r.Workspace, Phase: phase, Ready: running})
		dialer = dialer || (h.Name == SshGatewayIrohDial && running)
	}
	return dialer, nil
}

func exposuresReady(snap compose.Snapshot, desired []compose.Desired) bool {
	prefix := compose.ServiceNameFor(compose.ComponentKindSshServer, SshServerComponent)
	found := false
	for _, d := range desired {
		if d.Kind != compose.ComponentKindExposure || !strings.HasPrefix(d.Name, prefix) {
			continue
		}
		found = true
		if !snap.Ready(d.Kind, d.Name) {
			return false
		}
	}
	return found
}

func (p *pass) suspended(ctx context.Context, plan compose.AccountPlan) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ident, id, err := p.requesterIdentity(ctx, plan, false)
	if err != nil {
		return p.retry("the ssh requester's identity", err)
	}
	desired, err := p.desired(plan, ident, id)
	if err != nil {
		return p.fail(err)
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	held := compose.Existing(snap, desired)
	if err := p.ensure(ctx, held); err != nil {
		return p.retry("", err)
	}
	if _, err := p.hostProcesses(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = ident.Did
	compose.FillApplicationStatus(&p.st, o.Generation, true, o.Spec.Components, held, snap, nil)
	p.st = compose.Suspended(p.st)
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.SshGatewayGVR, p.st)
}

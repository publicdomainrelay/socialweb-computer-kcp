package sshgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/sshserver"
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
	dumbpipe := rec.Executables[DumbpipeExecutable].Path
	if dumbpipe == "" {
		return v, errors.New("sshgateway: the instance has no dumbpipe executable for the iroh dialer (dev up --dumbpipe-path in a dev build)")
	}
	v.IrohDial = manifests.IrohDial{Unit: l.Names().HostProcessPrefix() + SshGatewayIrohDial + ".service",
		Socket: IrohSocketPath(l), Dumbpipe: dumbpipe}
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
	c.ProvideTrust(schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind})
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
	p := &pass{ctx: ctx, c: c, r: r, o: o, st: compose.NewStatus(o.Generation, o.Spec.Profile, o.Status)}
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

func IrohSocketPath(l devinstance.Layout) string {
	inState := filepath.Join(compose.ComponentCRStateDir(l.StateDir, compose.ComponentKindSshServer, SshServerComponent), SshGatewayIrohSocket)
	if len(inState) <= irohdial.MaxSocketPath || l.RuntimeDir == "" {
		return inState
	}
	return filepath.Join(l.RuntimeDir, SshGatewayIrohSocket)
}

func CheckIrohSocket(l devinstance.Layout) error {
	socket := IrohSocketPath(l)
	if len(socket) > irohdial.MaxSocketPath {
		return fmt.Errorf("sshgateway: the iroh dialer socket %s is %d bytes, over the %d a unix socket path takes; pick a shorter --state-dir or --instance", socket, len(socket), irohdial.MaxSocketPath)
	}
	return nil
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
	mode, guestSSHKeysFile := compose.RequesterComponentFields(o.Spec.Options, plan.Mode)
	var requesterPdsRef *commonv1.ObjectRef
	if !plan.Local {
		requesterPdsRef = plan.PdsRef
	}
	requester := compose.Desired{Kind: compose.ComponentKindRequester, Name: marketrequester.RequesterComponent, Spec: v1alpha1.RequesterSpec{
		ComponentSpec:        p.c.NetworkComponentSpec(p.r, compose.ComponentCRStateDir(stateRoot, compose.ComponentKindRequester, marketrequester.RequesterComponent), network),
		Identity:             ident,
		PdsRef:               requesterPdsRef,
		PdsDID:               plan.PdsDID,
		OperatorDID:          compose.OperatorDID(o.Spec.Identity.ApprovedAssociations, id),
		PolicyEngineRef:      compose.ComponentRefFor(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent),
		Hostnames:            hostnames,
		Mode:                 mode,
		GuestSSHKeysFile:     guestSSHKeysFile,
		OwnerSessionsFile:    o.Spec.OwnerSessionsFile,
		IrohSocket:           IrohSocketPath(p.c.Config().Layout),
		MarketExposureRef:    requesterExposureRef(marketrequester.RequesterComponent),
		BidderDIDs:           append([]string(nil), o.Spec.BidderDids...),
		AcceptCopies:         int32(compose.OptionInt(o.Spec.Options, "accept-copies")),
		Comms:                o.Spec.Comms,
		Ingress:              o.Spec.RequesterIngress,
		SuspendParticipation: o.Spec.Suspend,
	}}
	sshServer := compose.Desired{Kind: compose.ComponentKindSshServer, Name: SshServerComponent, Spec: v1alpha1.SshServerSpec{
		ComponentSpec:     p.c.NetworkComponentSpec(p.r, compose.ComponentCRStateDir(stateRoot, compose.ComponentKindSshServer, SshServerComponent), network),
		Identity:          ident,
		RequesterRef:      compose.ComponentRefFor(compose.ComponentKindRequester, marketrequester.RequesterComponent),
		VaultRef:          vault,
		Hostnames:         hostnames,
		OwnerSessionsFile: o.Spec.OwnerSessionsFile,
		AccountRequesters: perUserAccounts(o.Spec),
		PublicListen:      publicListen(o.Spec),
		WebListen:         webListen(o.Spec),
		PublicOrigin:      compose.OptionValue(o.Spec.Options, compose.PublicOriginOption),
		OperatorDid:       operatorDid(o.Spec),
		MaxAccounts:       int32(maxAccounts(o.Spec)),
		PolicyEngineRef:   compose.ComponentRefFor(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent),
	}}
	out := []compose.Desired{compose.PolicyEngineDesired(), requester, sshServer}
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindRequester, marketrequester.RequesterComponent, "http", network, hostnames, false, true)...)
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindSshServer, SshServerComponent, "http", network, hostnames, true, false)...)
	out = append(out, compose.SSHExposures(ws, SshServerComponent)...)
	return compose.ApplyOverrides(o.Spec.Components, out)
}

func operatorDid(spec v1alpha1.SshGatewaySpec) string {
	if spec.Ssh == nil {
		return ""
	}
	return strings.TrimSpace(spec.Ssh.OperatorDid)
}

func webListen(spec v1alpha1.SshGatewaySpec) string {
	if spec.Ssh == nil {
		return ""
	}
	return strings.TrimSpace(spec.Ssh.WebListen)
}

func publicListen(spec v1alpha1.SshGatewaySpec) string {
	if spec.Ssh == nil {
		return ""
	}
	return strings.TrimSpace(spec.Ssh.PublicListen)
}

func perUserAccounts(spec v1alpha1.SshGatewaySpec) bool {
	return spec.OwnerSessionsFile != "" || compose.OptionValue(spec.Options, compose.PublicOriginOption) != ""
}

func requesterExposureRef(name string) *commonv1.LocalObjectRef {
	return &commonv1.LocalObjectRef{Name: compose.ServiceNameFor(compose.ComponentKindRequester, name) + compose.ExposureIngressSuffix}
}

type accountRequester struct {
	did string

	identity *v1alpha1.ComponentIdentity

	pdsRef *commonv1.ObjectRef

	pdsDID string

	links []compose.Link
}

func (p *pass) ownerSessionAccounts(ctx context.Context, base v1alpha1.RequesterSpec, mount string) ([]accountRequester, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	sessions, err := compose.ReadOwnerSessions(o.Spec.OwnerSessionsFile)
	if err != nil {
		return nil, err
	}
	var out []accountRequester
	for _, did := range slices.Sorted(maps.Keys(sessions)) {
		if len(out) >= maxAccounts(o.Spec) {
			break
		}
		s := sessions[did]
		owner := compose.Owner{DID: did, PdsURL: s.Service, Identifier: s.Identifier, Password: s.Password,
			CARef: c.PublishNetworkTrust(r, o.Spec.Network), Vault: SshGatewayVaultServer, Mount: mount}
		if err := c.ApplyOwnerRepo(ctx, r, ws, owner); err != nil {
			c.Log().Warn("composing an ssh account's repo", "root", r.Owner(), "did", did, "err", err.Error())
		}
		out = append(out, accountRequester{did: did, identity: &v1alpha1.ComponentIdentity{Did: did, Handle: strings.TrimSpace(s.Identifier)},
			pdsRef: base.PdsRef, pdsDID: base.PdsDID, links: compose.OwnerRepoLinks(ws, ws, owner)})
	}
	return out, nil
}

func withoutSharedRequester(desired []compose.Desired) []compose.Desired {
	prefix := compose.ServiceNameFor(compose.ComponentKindRequester, marketrequester.RequesterComponent)
	out := make([]compose.Desired, 0, len(desired))
	for _, d := range desired {
		switch {
		case d.Kind == compose.ComponentKindRequester && d.Name == marketrequester.RequesterComponent:
		case compose.IsExposureKind(d.Kind) && strings.HasPrefix(d.Name, prefix):
		default:
			out = append(out, d)
		}
	}
	return out
}

func (p *pass) accountRequesters(ctx context.Context, desired []compose.Desired, mount string) ([]compose.Desired, []compose.Link, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	if !perUserAccounts(o.Spec) {
		return nil, nil, nil
	}
	var base *v1alpha1.RequesterSpec
	for _, d := range desired {
		if spec, ok := d.Spec.(v1alpha1.RequesterSpec); ok && d.Kind == compose.ComponentKindRequester && d.Name == marketrequester.RequesterComponent {
			base = &spec
		}
	}
	if base == nil {
		return nil, nil, nil
	}
	server := compose.AppRef(compose.ComponentKindSshServer, SshServerComponent, ws)
	links := []compose.Link{compose.PolicyRunLink(server, compose.PolicyEngineRefIn(compose.PolicyEngineComponent, ws), sshserver.SignInWorkflow)}
	var accounts []accountRequester
	var err error
	if o.Spec.OwnerSessionsFile != "" {
		accounts, err = p.ownerSessionAccounts(ctx, *base, mount)
	} else {
		var sessionLinks []compose.Link
		accounts, sessionLinks, err = p.sessionOwnerAccounts(ctx)
		links = append(links, sessionLinks...)
	}
	if err != nil {
		return nil, nil, err
	}
	var out []compose.Desired
	for _, a := range accounts {
		name := contractmarket.SessionOwnerName(contractmarket.SessionOwnerRequesterPrefix, a.did)
		spec := *base
		spec.ComponentSpec = c.NetworkComponentSpec(r, compose.ComponentCRStateDir(c.StateRoot(), compose.ComponentKindRequester, name), o.Spec.Network)
		spec.Identity = a.identity
		spec.PdsRef, spec.PdsDID = a.pdsRef, a.pdsDID
		spec.OperatorDID, spec.GuestSSHKeysFile, spec.Hostnames = "", "", nil
		if a.identity.Handle != "" {
			spec.Hostnames = []string{a.identity.Handle}
		}
		if o.Spec.OwnerSessionsFile == "" {
			spec.OwnerSessionsFile = ""
			spec.Mode = compose.RequesterMode(v1alpha1.MarketIdentityModeOAuthSessionProxy)
		}
		spec.MarketExposureRef = requesterExposureRef(name)
		spec.ContractOwnerDid = a.did
		out = append(out, compose.Desired{Kind: compose.ComponentKindRequester, Name: name, Spec: spec})
		out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindRequester, name, "http", o.Spec.Network, spec.Hostnames, false, true)...)
		requester := compose.AppRef(compose.ComponentKindRequester, name, ws)
		links = append(links, a.links...)
		links = append(links,
			compose.PolicyRunLink(requester, compose.PolicyEngineRefIn(compose.PolicyEngineComponent, ws), marketrequester.Workflows...),
			compose.RepoWriteLink(requester, compose.RepoRefIn(a.did, ws), compose.ScopeAll),
			compose.RefLink(server, requester, compose.Cap(sshcontract.CapabilityRun, sshcontract.ScopeRun)))
	}
	return out, links, nil
}

func (p *pass) sessionOwnerAccounts(ctx context.Context) ([]accountRequester, []compose.Link, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	dids, err := c.SessionOwners(ctx, ws)
	if err != nil {
		return nil, nil, err
	}
	var keep []string
	for _, did := range dids {
		id, err := c.Identity(ctx, ws, compose.OwnerIdentityName(did))
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, err
		}
		if id != nil && id.Status.Phase == v1alpha1.MarketIdentityPhaseSessionExpired {
			c.Log().Info("the ssh gateway forgets an account whose session expired", "root", r.Owner(), "did", did)
			continue
		}
		if len(keep) >= maxAccounts(o.Spec) {
			c.Log().Warn("the ssh gateway holds its maximum number of accounts", "root", r.Owner(), "did", did, "max", maxAccounts(o.Spec))
			continue
		}
		keep = append(keep, did)
	}
	if err := c.PruneSessionOwners(ctx, ws, keep); err != nil {
		return nil, nil, err
	}
	var out []accountRequester
	var links []compose.Link
	for _, did := range keep {
		owner := compose.SessionOwner{DID: did, Vault: SshGatewayVaultServer, Scope: compose.SshGatewayAtprotoScope()}
		st, err := c.SessionOwnerState(ctx, ws, did)
		if err != nil {
			return nil, nil, err
		}
		links = append(links, compose.SessionOwnerLinks(r, ws, ws, owner, st.PdsURL)...)
		if err := c.ApplySessionOwner(ctx, r, ws, owner, st.PdsURL); err != nil {
			return nil, nil, err
		}
		if !st.Ready || st.PdsURL == "" {
			continue
		}
		id, err := c.Identity(ctx, ws, compose.OwnerIdentityName(did))
		if err != nil {
			return nil, nil, err
		}
		pdsRef, pdsDID := compose.SessionOwnerPds(st.PdsURL, "")
		identityRef := compose.AppRef(compose.KindMarketIdentity, id.Name, ws)
		out = append(out, accountRequester{did: did, identity: compose.ComponentIdentity(id, strings.TrimSpace(id.Status.Handle)), pdsRef: &pdsRef, pdsDID: pdsDID,
			links: []compose.Link{compose.ServiceAuthLink(compose.AppRef(compose.ComponentKindRequester,
				contractmarket.SessionOwnerName(contractmarket.SessionOwnerRequesterPrefix, did), ws), identityRef, did)}})
	}
	return out, links, nil
}

func maxAccounts(spec v1alpha1.SshGatewaySpec) int {
	if spec.Ssh != nil && spec.Ssh.MaxAccounts > 0 {
		return int(spec.Ssh.MaxAccounts)
	}
	return contractmarket.DefaultMaxSessionOwners
}

func (p *pass) ensure(ctx context.Context, desired []compose.Desired) error {
	return p.c.EnsureLinked(ctx, p.r, desired, desired)
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
		digest, digestErr := compose.InfraDigest(SshGatewaySet, o.Spec.Profile, &values, nil)
		if digestErr == nil && c.AppliedBefore(r, o.Status.AppliedSet, digest) {
			c.MarkApplied(r)
		} else {
			if _, err := p.wait("applying its own OpenBaoServer, its iroh dialer and its workspace"); err != nil {
				return ctlruntime.Result{}, err
			}
			if err := c.ApplyInfra(ctx, r, SshGatewaySet, o.Spec.Profile, &values); err != nil {
				return p.retry("", err)
			}
			c.MarkApplied(r)
			if digestErr == nil {
				p.st.AppliedSet = &v1alpha1.AppliedSet{Generation: o.Generation, Digest: digest}
			}
		}
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
	perUser := perUserAccounts(o.Spec)
	ident, id, err := p.requesterIdentity(ctx, plan, !perUser)
	if err != nil {
		return p.retry("the ssh requester's identity", err)
	}
	desired, err := p.desired(plan, ident, id)
	if err != nil {
		return p.fail(err)
	}
	accounts, accountLinks, err := p.accountRequesters(ctx, desired, mount)
	if err != nil {
		return p.retry("the signed-in accounts", err)
	}
	if perUser {
		desired = withoutSharedRequester(desired)
	}
	desired = append(desired, accounts...)
	if err := c.ApplyLinks(ctx, r, "accounts", accountLinks); err != nil {
		return ctlruntime.Result{}, err
	}
	if err := p.ensure(ctx, desired); err != nil {
		return p.retry("", err)
	}
	comms, force := compose.OverrideComms(o.Spec.Components, compose.ComponentKindRequester, marketrequester.RequesterComponent, o.Spec.Comms), compose.OAuthReceiveOnly(plan.Mode)
	streams, err := c.ApplyMarketStreams(ctx, r, compose.Streams{Role: compose.RequesterStreamRole, Comms: comms,
		Sources: compose.SourceInput{Network: o.Spec.Network}, ForceFirehose: force,
		Public: compose.RequesterPublicSources(comms, o.Spec.Network, compose.RequesterCommsMode(comms, force))})
	if err != nil {
		return p.fail(err)
	}
	if err := c.ApplyLinks(ctx, r, "events", streams.Links); err != nil {
		return ctlruntime.Result{}, err
	}
	if _, err := c.DeleteComponentsExcept(ctx, ws, r.Owner(), compose.DesiredKeys(desired)); err != nil {
		return ctlruntime.Result{}, err
	}
	var repoLinks []compose.Link
	if ident.Did != "" && !perUser {
		if id != nil && !plan.Local {
			accountLinks, err := c.AccountRepo(ctx, r, ws, marketrequester.RequesterIdentity, plan, id.Status.Did, id.Status.PdsURL)
			if err != nil {
				c.Log().Warn("the existing-account repo", "root", r.Owner(), "err", err.Error())
			}
			repoLinks = append(repoLinks, accountLinks...)
			repoLinks = append(repoLinks, compose.ServiceAuthLink(compose.AppRef(compose.ComponentKindRequester, marketrequester.RequesterComponent, ws),
				compose.AppRef(compose.KindMarketIdentity, marketrequester.RequesterIdentity, ws), id.Status.Did))
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
	if err := c.ApplyOAuthIdentityEndpoints(ctx, r, snap, r.Workspace, marketrequester.RequesterIdentity,
		compose.ServiceNameFor(compose.ComponentKindRequester, marketrequester.RequesterComponent)+compose.ExposureIngressSuffix, "requester", o.Spec.Identity); err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = ident.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	dialer, err := p.hostProcesses(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	switch {
	case snap.Satisfied(desired) && exposuresReady(snap, desired) && dialer && (perUser || ident.Did != ""):
		p.st = compose.Ready(p.st, "the ssh gateway listens, its iroh dialer runs and its owner identity is known")
		return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.SshGatewayGVR, p.st)
	case ident.Did == "" && !perUser:
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
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if _, err := p.hostProcesses(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = ident.Did
	compose.FillApplicationStatus(&p.st, o.Generation, true, o.Spec.Components, held, snap, nil)
	p.st = compose.SuspendedOnceApplied(p.st, snap, held)
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.SshGatewayGVR, p.st)
}

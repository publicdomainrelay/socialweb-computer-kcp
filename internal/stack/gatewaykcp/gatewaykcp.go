package gatewaykcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcertfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devcore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewaymain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/providermain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/denoruntime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tsbundle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultmain"
)

const (
	InstanceID         = "gateway"
	FieldManager       = "socialweb-computer"
	ApplyTimeout       = time.Minute
	VaultTimeout       = time.Minute
	ListenTimeout      = 30 * time.Second
	Poll               = 500 * time.Millisecond
	ListenerXRPC       = "xrpc"
	ListenerTLS        = "tls"
	ListenerClientCert = "client-cert"
	ListenerWebApp     = "web-app-front"
	ComponentCore      = "core"
	DefaultAddress     = "127.0.0.1:0"
	ExeCore            = "socialweb-computer"
	ExeKcp             = "kcp"
	ExeKine            = "kine"
	ExePostgres        = "postgres"
	ExeBao             = "bao"
	ExeDeno            = "deno"
	ExeExternalSecrets = "external-secrets"
	ExeESOBundle       = "external-secrets-bundle"
	ExeDumbpipe        = "dumbpipe"
	ExeK3s             = "k3s"
	ExeWebDownloads    = "socialweb-computer-k8s-downloads"
	recordProfile      = bootstrap.ProfileProdlike
	componentsReport   = "members"
)

var Sets = []string{"provider", "permission-inventory", "hostprocesses", appctl.ManifestSet}

type Config struct {
	StateDir            string
	Prefix              string
	Backend             hostsvc.Backend
	Executables         map[string]devinstance.Executable
	Closure             tsbundle.Prepared
	Spec                v1alpha1.GatewaySpec
	FrontAddress        string
	TLSAddress          string
	CertFrontAddress    string
	TLSCert             string
	TLSKey              string
	BootstrapCluster    string
	BootstrapAccountDID string
	RootName            string
	ReadyTimeout        time.Duration
	Environ             []string
	Unit                string
	Exe                 string
	Stderr              io.Writer
	Log                 *slog.Logger
}

func NewLayout(state, prefix string) devinstance.Layout {
	kcp := filepath.Join(state, "kcp")
	run := filepath.Join(kcp, "run")
	return devinstance.Layout{ID: InstanceID, Short: table.GatewayKey, StateDir: state, RuntimeDir: run,
		Socket: filepath.Join(run, "sup.sock"), SupState: filepath.Join(state, "sup"),
		Record: filepath.Join(state, "gateway.json"), CoreRecord: filepath.Join(state, "core.json"),
		Lock: filepath.Join(kcp, "instance.lock"), CoreLock: filepath.Join(kcp, "core.lock"), Logs: filepath.Join(state, "logs"),
		KineDir: filepath.Join(kcp, "kine-a"), KcpRoot: filepath.Join(kcp, "slot-a"), Prefix: prefix}
}

func (c Config) app() table.App {
	a := table.Gateway()
	if c.Prefix != "" {
		a.Prefix = c.Prefix
	}
	return a
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "serve-gateway-kcp")
}

func (c Config) linkAssets(l devinstance.Layout) (map[string]devinstance.Executable, error) {
	p := bootstrap.InstallParams(c.app(), c.StateDir)
	out := map[string]devinstance.Executable{}
	for k, v := range c.Executables {
		out[k] = v
	}
	kcp, kine, pg := c.Executables[ExeKcp].Path, c.Executables[ExeKine].Path, c.Executables[ExePostgres].Path
	if kcp != "" && kine != "" && pg != "" {
		if err := bootstrap.LinkAssets(p, kcp, kine, pg); err != nil {
			return nil, err
		}
	}
	for name, link := range map[string]string{ExeKcp: p.KcpPath, ExeKine: p.KinePath, ExePostgres: p.PostgresRoot} {
		if _, err := os.Stat(link); err != nil {
			return nil, fmt.Errorf("gatewaykcp: %s is not linked at %s (install links it; a dev build names it with its -path option): %w", name, link, err)
		}
		e := out[name]
		e.Path = link
		out[name] = e
	}
	return out, nil
}

func (c Config) record(l devinstance.Layout) (devinstance.Record, error) {
	var rec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &rec); err != nil {
		rec = devinstance.Record{Created: time.Now().UTC()}
	}
	exes, err := c.linkAssets(l)
	if err != nil {
		return rec, err
	}
	rec.Layout, rec.Profile, rec.Backend, rec.Executables = l, recordProfile, c.Backend, exes
	rec.BaseWorkspace, rec.Host, rec.Parent, rec.StandIns, rec.OpenBao = "", false, nil, nil, nil
	return rec, nil
}

type listeners struct {
	mu     sync.Mutex
	addr   map[string]string
	kind   map[string]string
	webApp string
}

func (r *listeners) components(_ string, ls []v1alpha1.ApplicationListener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webApp = ""
	for _, l := range ls {
		if l.Component == appctl.WebAppComponent && l.Address != "" {
			r.webApp = l.Address
		}
	}
}

func (r *listeners) webAppAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.webApp
}

func (r *listeners) set(name, kind string, a net.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addr[name], r.kind[name] = a.String(), kind
}

func (r *listeners) get(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr[name]
}

func (r *listeners) report(string) []v1alpha1.ApplicationListener {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.addr))
	for n := range r.addr {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []v1alpha1.ApplicationListener{}
	for _, n := range names {
		out = append(out, v1alpha1.ApplicationListener{Component: ComponentCore, Name: n, Kind: r.kind[n], Address: r.addr[n]})
	}
	return out
}

func Run(ctx context.Context, c Config, m hostsvc.Manager) error {
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("gatewaykcp: state dir %q is not absolute", c.StateDir)
	}
	l := NewLayout(c.StateDir, c.Prefix)
	for _, d := range []string{l.StateDir, l.KcpDir(), l.RuntimeDir, l.Logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if n := len(filepath.Join(l.RuntimeDir, "kine-b.sock")); n > devinstance.MaxSocketPath {
		return fmt.Errorf("gatewaykcp: the runtime dir %s makes socket paths of %d bytes, over %d", l.RuntimeDir, n, devinstance.MaxSocketPath)
	}
	lk, err := os.OpenFile(l.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	lk.Close()
	rec, err := c.record(l)
	if err != nil {
		return err
	}
	if err := devinstance.WriteJSON(l.Record, rec); err != nil {
		return err
	}
	app := c.app()
	c.log().Info("the gateway application instance starts: lock, kcp store, kine, front, active slot, sets, root vault, members",
		"state", c.StateDir, "prefix", l.UnitPrefix(), "backend", string(c.Backend), "units", app.Names().UnitPrefix())
	deno := denoruntime.Config{StateDir: c.StateDir, Deno: c.Executables[ExeDeno].Path, Kubeconfig: l.FrontKubeconfig(), Closure: c.Closure,
		Unit: c.Unit, ApplyTimeout: ApplyTimeout, Log: c.Log, EnforceMemory: c.Backend == hostsvc.BackendSystemdUser}
	return run(ctx, c, l, &deno, &app, m)
}

func RunChild(ctx context.Context, c Config, l devinstance.Layout, deno denoruntime.Config, m hostsvc.Manager) error {
	c.StateDir = l.StateDir
	if err := os.MkdirAll(l.KcpDir(), 0o700); err != nil {
		return err
	}
	if c.RootName == "" {
		c.RootName = appctl.ChildRootName
	}
	c.log().Info("a gateway child application of a market network starts: its workspace of the network's kcp, sets, root vault, members",
		"state", c.StateDir, "instance", l.ID, "root", c.RootName)
	return run(ctx, c, l, &deno, nil, m)
}

func run(ctx context.Context, c Config, l devinstance.Layout, deno *denoruntime.Config, app *table.App, m hostsvc.Manager) error {
	reg := &listeners{addr: map[string]string{}, kind: map[string]string{}}
	g := &gateway{c: c, l: l, reg: reg}
	if n := c.Spec.Network; n != nil && n.CaBundleRef != "" && os.Getenv("SSL_CERT_FILE") == "" {
		bundle, err := outboundCABundle(filepath.Join(c.StateDir, "tls"), n.CaBundleRef)
		if err != nil {
			return err
		}
		os.Setenv("SSL_CERT_FILE", bundle)
		c.log().Info("the system roots and the market network's CA are this gateway's outbound trust: every vault it places verifies a bidder's issuer with them",
			"bundle", bundle, "networkCA", n.CaBundleRef)
	}
	if c.Spec.Network != nil {
		m, ok, err := guestTLSEgress(c.Spec.Network.GuestTlsUrl)
		if err != nil {
			return err
		}
		if ok {
			addr, err := runEgress(ctx, m, filepath.Join(l.KcpDir(), egressRecord), c.log())
			if err != nil {
				return err
			}
			reg.set(ListenerEgress, "http", addr)
			os.Setenv("HTTPS_PROXY", "http://"+addr.String())
			c.log().Info("the market network's guest TLS front is this gateway's egress for its port-less names: every vault it places fetches a bidder's issuer through it",
				"egress", addr.String(), "names", "*."+m.host+":443", "target", m.target)
		}
	}
	return devcore.Run(ctx, devcore.Config{Layout: l, ReadyTimeout: c.ReadyTimeout, Log: c.Log, Application: app, FrontAddress: c.FrontAddress,
		DenoRuntime: deno, WebApp: &appctl.WebApp{FrontKubeconfig: g.memberKubeconfig(),
			ClusterCADir: filepath.Join(c.StateDir, "clusterprovider", clusterstack.PublicCADirName),
			DownloadsDir: c.Executables[ExeWebDownloads].Path},
		OnComponents: reg.components, Before: g.before, Members: g.members, Listeners: reg.report}, m)
}

type gateway struct {
	c   Config
	l   devinstance.Layout
	kcp devinstance.Layout
	ws  string
	reg *listeners
}

func (g *gateway) adopt(rec devinstance.Record) {
	g.kcp, g.ws = g.l, rec.BaseWorkspace
	if rec.SharedKcp != "" {
		g.kcp = devinstance.Layout{StateDir: rec.SharedKcp}
	}
	if g.ws == "" {
		g.ws = appctl.DefaultWorkspace
	}
}

func clusterConfig(base *rest.Config, ws string) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.Host = cfg.Host + "/clusters/" + ws
	return cfg
}

func (g *gateway) before(ctx context.Context, rec devinstance.Record) error {
	started := time.Now()
	g.adopt(rec)
	restCfg, err := kcpserver.FrontRestConfig(rec.AdminKubeconfig)
	if err != nil {
		return err
	}
	ap, err := kcpapply.New(restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, Base: g.ws, Log: g.c.Log})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	for _, name := range Sets {
		set, ok := ix.Set(name)
		if !ok {
			return fmt.Errorf("gatewaykcp: deploy/sets.yaml names no %s set", name)
		}
		values, err := manifests.DecodeValues(set.Values, nil)
		if err != nil {
			return err
		}
		if _, err := ap.Apply(ctx, set, values); err != nil {
			return fmt.Errorf("gatewaykcp: applying the %s set: %w", name, err)
		}
	}
	if err := writeMemberKubeconfig(g.activeSlotRoot(), rec.KcpURL, g.memberKubeconfig()); err != nil {
		return err
	}
	if err := applyRoot(ctx, restCfg, g.ws, g.rootName(), g.c.Spec); err != nil {
		return err
	}
	g.c.log().Info("the sets and the Gateway root object are applied", "sets", Sets, "profile", g.c.Spec.Profile,
		"seconds", time.Since(started).Seconds())
	return nil
}

func (g *gateway) memberKubeconfig() string { return filepath.Join(g.l.KcpDir(), MemberKubeconfig) }

func (g *gateway) rootName() string {
	if g.c.RootName != "" {
		return g.c.RootName
	}
	return appctl.GatewayRootName
}

func applyRoot(ctx context.Context, restCfg *rest.Config, ws, name string, spec v1alpha1.GatewaySpec) error {
	dc, err := dynamic.NewForConfig(clusterConfig(restCfg, ws))
	if err != nil {
		return err
	}
	o := v1alpha1.Gateway{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: string(v1alpha1.ApplicationKindGateway)},
		ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&o)
	if err != nil {
		return err
	}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	body, err := jsonMarshal(m)
	if err != nil {
		return err
	}
	force := true
	_, err = dc.Resource(appctl.GatewayGVR).Patch(ctx, name, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func (g *gateway) rootVault(ctx context.Context, rec devinstance.Record) (vaultmain.RootAccess, error) {
	restCfg, err := kcpserver.FrontRestConfig(rec.AdminKubeconfig)
	if err != nil {
		return vaultmain.RootAccess{}, err
	}
	dc, err := dynamic.NewForConfig(clusterConfig(restCfg, g.ws))
	if err != nil {
		return vaultmain.RootAccess{}, err
	}
	deadline := time.Now().Add(VaultTimeout)
	last := "not declared yet"
	for {
		u, err := dc.Resource(openbaoctl.GVR).Get(ctx, vaultlayout.RootServerName, metav1.GetOptions{})
		if err == nil {
			var o v1alpha1.OpenBaoServer
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
				return vaultmain.RootAccess{}, err
			}
			if appctl.RootVaultSynced(&o) {
				return vaultmain.RootAccessOf(o)
			}
			last = fmt.Sprintf("%s %s", o.Status.Phase, o.Status.Message)
			if o.Status.Phase == v1alpha1.OpenBaoServerPhaseFailed && o.Status.ObservedGeneration == o.Generation {
				return vaultmain.RootAccess{}, fmt.Errorf("gatewaykcp: the root vault %s failed: %s", vaultlayout.RootServerName, last)
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return vaultmain.RootAccess{}, fmt.Errorf("gatewaykcp: the root vault %s was not Ready and synced within %s: %s",
				vaultlayout.RootServerName, VaultTimeout, last)
		}
		select {
		case <-ctx.Done():
			return vaultmain.RootAccess{}, ctx.Err()
		case <-time.After(Poll):
		}
	}
}

func (g *gateway) applyRoot(ctx context.Context, rec devinstance.Record, vcfg vaultmain.Config) (vaultmain.RootAccess, error) {
	deadline := time.Now().Add(VaultTimeout)
	for {
		access, err := g.rootVault(ctx, rec)
		if err != nil {
			return access, err
		}
		err = vaultmain.ApplyRoot(ctx, access, vcfg, g.c.Log.With("component", "apply-root"))
		if err == nil {
			return access, nil
		}
		if time.Now().After(deadline) {
			return access, fmt.Errorf("gatewaykcp: apply-root as client %s: %w", vaultlayout.RootClient, err)
		}
		g.c.log().Warn("apply-root did not reach the root vault; its status may name a process that is gone while the core places it again",
			"address", access.Address, "err", err.Error())
		select {
		case <-ctx.Done():
			return access, ctx.Err()
		case <-time.After(4 * Poll):
		}
	}
}

func (g *gateway) activeSlotRoot() string {
	s, err := kcpserver.ReadSlots(g.kcp.SlotsFile())
	if err != nil || s.Active == "" {
		return g.kcp.SlotRoot(table.SlotA)
	}
	if r, ok := s.Slots[s.Active]; ok && r.Root != "" {
		return r.Root
	}
	return g.kcp.SlotRoot(s.Active)
}

type member struct {
	name string
	run  func(ctx context.Context) error
}

func (g *gateway) members(ctx context.Context, rec devinstance.Record) error {
	log := g.c.log()
	cert, key := g.c.TLSCert, g.c.TLSKey
	if cert == "" || key == "" {
		var err error
		if cert, key, err = tlsfront.EnsureSelfSigned(filepath.Join(g.c.StateDir, "tls")); err != nil {
			return err
		}
	}
	env := newEnv(g.c.Environ)
	if err := g.defaults(env, rec, cert, key); err != nil {
		return err
	}
	vcfg := vaultmain.Config{KCPKubeconfig: rec.AdminKubeconfig, KCPRootDir: g.activeSlotRoot(), Workspace: g.ws,
		Server:  vaultlayout.RootServerName,
		TLSCert: cert, TLSKey: key}
	access, err := g.applyRoot(ctx, rec, vcfg)
	if err != nil {
		return err
	}
	log.Info("the root vault holds the root layout", "address", access.Address)
	if g.c.BootstrapCluster != "" {
		if err := bootstrapCluster(ctx, g.memberKubeconfig(), g.ws, g.c.BootstrapAccountDID, g.c.BootstrapCluster, log); err != nil {
			return err
		}
	}
	if code := providermain.Main(ctx, g.invocation(env, []string{"migrate"}, "migrate")); code != 0 {
		log.Warn("the legacy-key migration did not complete; the gateway starts anyway, the clusterprovider runs the same migration itself", "exit", code)
	}
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go followMemberToken(mctx, g.activeSlotRoot, filepath.Join(g.l.KcpDir(), MemberToken), log)
	ended := make(chan error, 8)
	start := func(m member) {
		go func() {
			err := m.run(mctx)
			if err == nil && mctx.Err() == nil {
				err = errors.New("returned")
			}
			if err != nil && mctx.Err() == nil {
				err = fmt.Errorf("%s: %w", m.name, err)
			}
			ended <- err
		}()
	}
	denyCfg, err := clientcmd.BuildConfigFromFlags("", g.memberKubeconfig())
	if err != nil {
		return fmt.Errorf("gatewaykcp: reading the member kubeconfig: %w", err)
	}
	kcpserver.FollowTokenFile(denyCfg)
	deny, err := kcpclient.New(kcpclient.Options{Host: rec.KcpURL, RestConfig: denyCfg})
	if err != nil {
		return err
	}
	certFrontAddress := g.c.CertFrontAddress
	if certFrontAddress == "" {
		certFrontAddress = DefaultAddress
	}
	start(member{"client certificate front", func(ctx context.Context) error {
		return clientcertfront.Run(ctx, clientcertfront.Config{Address: certFrontAddress, CertFile: cert, KeyFile: key,
			EdgeDir: filepath.Join(g.c.StateDir, "tls"), ClusterStateDir: env.get("SOCIALWEB_CLUSTERPROVIDER_STATE_DIR"), Deny: deny, Records: deny,
			OnListen: func(a net.Addr) { g.reg.set(ListenerClientCert, "https", a) }, Log: g.c.Log})
	}})
	certFront, err := g.waitListener(mctx, ended, ListenerClientCert)
	if err != nil {
		return err
	}
	if env.get("SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST") == "" {
		env.set("SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST", "https://"+certFront)
		env.dflt("SOCIALWEB_GATEWAY_KCP_PUBLIC_CA_FILE", cert)
	}
	start(member{"gateway", func(ctx context.Context) error {
		inv := g.invocation(env, nil, "serve", "gateway", "xrpc")
		inv.Listen = func(_ string, a net.Addr) { g.reg.set(ListenerXRPC, "http", a) }
		return exitErr(gatewaymain.Main(ctx, inv))
	}})
	gw, err := g.waitListener(mctx, ended, ListenerXRPC)
	if err != nil {
		return err
	}
	start(member{"tls front", func(ctx context.Context) error {
		return tlsfront.Run(ctx, tlsfront.Config{Address: g.c.TLSAddress, CertFile: cert, KeyFile: key,
			Backend: func() string { return g.reg.get(ListenerXRPC) }, OnListen: func(a net.Addr) { g.reg.set(ListenerTLS, "https", a) },
			Log: g.c.Log})
	}})
	start(member{"web app front", func(ctx context.Context) error {
		return tlsfront.Run(ctx, tlsfront.Config{Address: g.c.Spec.WebAppFront, Backend: g.reg.webAppAddress,
			OnListen: func(a net.Addr) { g.reg.set(ListenerWebApp, "tcp", a) }, Log: g.c.Log})
	}})
	start(member{"clusterprovider", func(ctx context.Context) error {
		return exitErr(providermain.Main(ctx, g.invocation(env, nil, "serve", "provider")))
	}})
	log.Info("the gateway's members run in this process; the app controller renders the "+appctl.WebAppComponent+" web app as a DenoPod", "xrpc", gw)
	err = <-ended
	cancel()
	if err == nil {
		return ctx.Err()
	}
	return err
}

func exitErr(code int) error {
	if code == 0 {
		return nil
	}
	return fmt.Errorf("exit %d", code)
}

func (g *gateway) waitListener(ctx context.Context, ended <-chan error, name string) (string, error) {
	deadline := time.After(ListenTimeout)
	for {
		if a := g.reg.get(name); a != "" {
			return a, nil
		}
		select {
		case err := <-ended:
			return "", fmt.Errorf("gatewaykcp: %s ended before it listened: %v", name, err)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("gatewaykcp: %s reported no listener within %s", name, ListenTimeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (g *gateway) invocation(e *env, args []string, verb ...string) foldmain.Invocation {
	return foldmain.Invocation{Args: args, Getenv: e.get, Stdout: g.c.Stderr, Stderr: g.c.Stderr, Exe: g.c.Exe, Verb: verb}
}

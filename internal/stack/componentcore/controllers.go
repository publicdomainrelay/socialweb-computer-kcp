package componentcore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/recordcontroller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/childapplication"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	eventsctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/events"
	policyctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policy"
	remotepdsctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/remotepds"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	vaultctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/leasecontroller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketpresence"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	policycontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/remotepds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultmain"
)

const (
	controllerResync = 30 * time.Second

	pdsClientTimeout = 30 * time.Second
)

type controllerRunner struct {
	name string

	run func(context.Context) error
}

type reconcileFunc func(context.Context, dynamic.Interface, string, *unstructured.Unstructured) error

type controllerEntry struct {
	gvr schema.GroupVersionResource

	core bool

	reconcile reconcileFunc
}

type controllerRuntime struct {
	dynamic DynamicFactory

	workspaces func(context.Context) []string

	coreWorkspace string

	log *slog.Logger

	retry time.Duration

	resync time.Duration

	mu sync.Mutex

	entries []controllerEntry
}

func (r *controllerRuntime) add(entry controllerEntry) error {
	if entry.gvr.Resource == "" {
		return errors.New("componentcore: a controller entry needs a resource")
	}
	if entry.reconcile == nil {
		return fmt.Errorf("componentcore: the controller for %s has no reconciler", entry.gvr.Resource)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.entries {
		if existing.gvr == entry.gvr {
			return fmt.Errorf("componentcore: the controller for %s is registered twice", entry.gvr.Resource)
		}
	}
	r.entries = append(r.entries, entry)
	return nil
}

func (r *controllerRuntime) Run(ctx context.Context) error {
	r.mu.Lock()
	entries := append([]controllerEntry(nil), r.entries...)
	r.mu.Unlock()
	var workers sync.WaitGroup
	for _, entry := range entries {
		workspaces := r.workspaces(ctx)
		if entry.core {
			workspaces = []string{r.coreWorkspace}
		}
		seen := map[string]bool{}
		for _, workspace := range workspaces {
			workspace = strings.TrimSpace(workspace)
			if workspace == "" || seen[workspace] {
				continue
			}
			seen[workspace] = true
			workers.Go(func() {
				r.watch(ctx, entry, workspace)
			})
		}
	}
	workers.Wait()
	return nil
}

func (r *controllerRuntime) watch(ctx context.Context, entry controllerEntry, workspace string) {
	log := r.log.With("workspace", workspace, "resource", entry.gvr.Resource)
	for ctx.Err() == nil {
		dyn, err := r.dynamic(workspace)
		if err != nil {
			log.Warn("the controller has no dynamic client", "err", err.Error())
			if !clock.Sleep(ctx, r.retry) {
				return
			}
			continue
		}
		if dyn == nil {
			log.Warn("the controller has a nil dynamic client")
			if !clock.Sleep(ctx, r.retry) {
				return
			}
			continue
		}
		if err := r.round(ctx, entry, workspace, dyn); err != nil && ctx.Err() == nil {
			log.Warn("watching the resource", "err", err.Error())
		}
		if !clock.Sleep(ctx, r.retry) {
			return
		}
	}
}

func (r *controllerRuntime) round(ctx context.Context, entry controllerEntry, workspace string, dyn dynamic.Interface) error {
	ri := dyn.Resource(entry.gvr)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		r.reconcile(ctx, entry, workspace, dyn, &list.Items[i])
	}
	wc, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer wc.Stop()
	timer := time.NewTimer(r.resync)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case event, ok := <-wc.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Bookmark || event.Type == watch.Deleted {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil {
				continue
			}
			r.reconcile(ctx, entry, workspace, dyn, u)
		}
	}
}

func (r *controllerRuntime) reconcile(ctx context.Context, entry controllerEntry, workspace string, dyn dynamic.Interface,
	u *unstructured.Unstructured) {
	if u == nil {
		return
	}
	if err := entry.reconcile(ctx, dyn, workspace, u); err != nil && ctx.Err() == nil {
		r.log.Warn("reconciling a controller object", "resource", entry.gvr.Resource, "workspace", workspace,
			"name", u.GetName(), "err", err.Error())
	}
}

type leaseRegistrar struct {
	runtime *controllerRuntime
}

func (l leaseRegistrar) Register(gvr schema.GroupVersionResource, reconcile leasecontroller.ReconcileFunc) error {
	core := gvr == leasecontroller.ClusterGVR || gvr == leasecontroller.ClusterRollGVR
	return l.runtime.add(controllerEntry{gvr: gvr, core: core,
		reconcile: func(ctx context.Context, _ dynamic.Interface, workspace string, u *unstructured.Unstructured) error {
			return reconcile(ctx, workspace, u)
		}})
}

type childRegistrar struct {
	runtime *controllerRuntime
}

func (c childRegistrar) Register(gvr schema.GroupVersionResource, reconcile childapplication.ReconcileFunc) error {
	return c.runtime.add(controllerEntry{gvr: gvr, core: true,
		reconcile: func(ctx context.Context, dyn dynamic.Interface, _ string, u *unstructured.Unstructured) error {
			return reconcile(ctx, dyn, u)
		}})
}

type hostHooks struct {
	runtime *controllerRuntime
}

func (h hostHooks) Add(gvr schema.GroupVersionResource, reconcile hostlifecycle.ReconcileFunc) {
	err := h.runtime.add(controllerEntry{gvr: gvr, core: true,
		reconcile: func(ctx context.Context, dyn dynamic.Interface, _ string, u *unstructured.Unstructured) error {
			return reconcile(ctx, dyn, u)
		}})
	if err != nil {
		h.runtime.log.Warn("the host lifecycle controller is not registered", "resource", gvr.Resource, "err", err.Error())
	}
}

func (c *Core) workspaceList(ctx context.Context) []string {
	if c.cfg.WorkspacesFunc != nil {
		return c.cfg.WorkspacesFunc(ctx)
	}
	return c.cfg.Workspaces
}

func (c *Core) buildControllers() {
	cfg := c.cfg
	base := cfg.Workspaces[0]
	log := cfg.Log
	runtime := &controllerRuntime{dynamic: cfg.DynamicFactory, workspaces: c.workspaceList, coreWorkspace: base,
		log: log.With("component", "controller-runtime"), retry: RetryAfter, resync: controllerResync}
	c.controllers = runtime

	subnets, err := subnetallocator.Open(subnetallocator.Config{LeasePath: filepath.Join(cfg.StateDir, "networks.json"),
		AppInstance: cfg.InstallAccount})
	if err != nil {
		log.Warn("the lease controller is not wired: no subnet allocator", "err", err.Error())
	} else {
		leaseOptions := leasecontroller.Options{
			Dynamic: leasecontroller.DynamicFactory(cfg.DynamicFactory),
			Ports:   c.ports,
			Subnets: subnets,
			Rolls: func(string) (kcpserver.Slots, error) {
				return kcpserver.ReadSlots(cfg.SlotsFile)
			},
			Front: func(string) (leasecontroller.FrontState, error) {
				return apiserverfront.Client{Path: cfg.FrontControl}, nil
			},
		}
		if err := leasecontroller.Register(leaseRegistrar{runtime: runtime}, leaseOptions); err != nil {
			log.Warn("the lease controller is not wired", "err", err.Error())
		}
		if leaseCtl, err := leasecontroller.New(leaseOptions); err != nil {
			log.Warn("the lease controller mirror is not wired", "err", err.Error())
		} else {
			c.runners = append(c.runners, controllerRunner{name: "lease-mirror",
				run: func(ctx context.Context) error { return c.mirrorLeases(ctx, leaseCtl) }})
		}
	}

	authority := cfg.HostAuthority
	if authority == nil {
		authority = hostlifecycle.NewFileAuthority(hostlifecycle.FileAuthorityOptions{
			StateRoot:    cfg.InstallStateRoot,
			StateDir:     cfg.StateDir,
			Account:      cfg.InstallAccount,
			SlotsFile:    cfg.SlotsFile,
			FrontControl: cfg.FrontControl,
		})
	}
	if ctl, err := hostlifecycle.New(hostlifecycle.Options{Authority: authority, Log: log}); err != nil {
		log.Warn("the host lifecycle controller is not wired", "err", err.Error())
	} else {
		hostlifecycle.Register(hostHooks{runtime: runtime}, ctl)
	}

	if _, err := childapplication.Register(childRegistrar{runtime: runtime}, childapplication.Options{Dynamic: childapplication.DynamicFactory(cfg.DynamicFactory),
		Workspace: base, NetworkWorkspace: base, Log: log}); err != nil {
		log.Warn("the child application controller is not wired", "err", err.Error())
	}

	secrets := coreSecretResolver(c.store, base, cfg.CredentialClient, cfg.CredentialServer)
	caBundle := func(ref string) ([]byte, error) { return os.ReadFile(openbaoctl.FileRef(ref)) }
	if strings.TrimSpace(base) == DefaultWorkspace {
		c.buildManager(caBundle)
	}
	c.buildCompute()

	handlers := c.handlers
	if handlers == nil {
		handlers = component.NewHandlerRegistry()
		c.handlers = handlers
	}
	repoResolver := &pdsclient.RepoResolver{Handlers: handlers, Timeout: pdsClientTimeout, Secrets: secrets,
		RemotePDS: remotePDSSource(cfg.DynamicFactory), CABundle: caBundle}
	if repoCtl, err := appctl.NewAtprotoRepoController(appctl.AtprotoRepoOptions{Resolver: repoResolver, Log: log}); err != nil {
		log.Warn("the AtprotoRepo controller is not wired", "err", err.Error())
	} else if err := runtime.add(controllerEntry{gvr: statusstate.AtprotoRepoGVR,
		reconcile: func(ctx context.Context, dyn dynamic.Interface, workspace string, u *unstructured.Unstructured) error {
			return repoCtl.Reconcile(ctx, dyn, workspace, u)
		}}); err != nil {
		log.Warn("the AtprotoRepo controller is not registered", "err", err.Error())
	}
	if recordCtl, err := recordcontroller.New(recordcontroller.Options{Resolver: repoResolver,
		Dynamic: recordcontroller.DynamicFactory(cfg.DynamicFactory), Log: log, AddFinalizer: true}); err != nil {
		log.Warn("the AtprotoRecord controller is not wired", "err", err.Error())
	} else if err := runtime.add(controllerEntry{gvr: recordcontroller.AtprotoRecordGVR,
		reconcile: func(ctx context.Context, dyn dynamic.Interface, workspace string, u *unstructured.Unstructured) error {
			return recordCtl.Reconcile(ctx, dyn, workspace, u)
		}}); err != nil {
		log.Warn("the AtprotoRecord controller is not registered", "err", err.Error())
	}
	if mp, err := marketpresence.NewController(marketpresence.Options{Workspaces: cfg.Workspaces, Dynamic: marketpresence.DynamicFactory(cfg.DynamicFactory),
		HTTPClient: handlers.Client(), Log: log}); err != nil {
		log.Warn("the market presence controller is not wired", "err", err.Error())
	} else {
		c.runners = append(c.runners, controllerRunner{name: "marketpresence", run: mp.Run})
	}

	resolver := &pdsclient.Resolver{Handlers: handlers, Timeout: pdsClientTimeout}
	plcBaseURL := c.plcBaseURL()
	if ss, err := statusstate.New(statusstate.Options{Workspace: base, Dynamic: statusstate.DynamicFactory(cfg.DynamicFactory),
		PDS: statusPDSResolver(resolver, base), RepoResolver: repoResolver, Vault: openBaoMetadata(c.store, base),
		PLCBaseURL: plcBaseURL, PLCHandlers: handlers,
		RelayEndpoint: c.relayEndpoint(base), RelayHandlers: handlers, Log: log}); err != nil {
		log.Warn("the status state controller is not wired", "err", err.Error())
	} else {
		c.runners = append(c.runners, controllerRunner{name: "statusstate", run: ss.Run})
	}
}

func (c *Core) buildManager(caBundle remotepdsctl.CABundleFunc) {
	cfg := c.cfg
	log := cfg.Log
	restCfg, err := c.managerRestConfig()
	if err != nil {
		log.Warn("the controller manager is not wired", "err", err.Error())
		return
	}
	mgr, err := ctlruntime.NewManager(ctlruntime.Config{Rest: restCfg, AddToScheme: []func(*runtime.Scheme) error{pdsv1alpha1.AddToScheme, policyv1alpha1.AddToScheme, v1alpha1.AddToScheme,
		eventsv1alpha1.AddToScheme},
		Log: log})
	if err != nil {
		log.Warn("the controller manager is not wired", "err", err.Error())
		return
	}
	secrets := func(workspace string) pdsclient.SecretResolver {
		return coreSecretResolver(c.store, workspace, cfg.CredentialClient, cfg.CredentialServer)
	}
	if _, err := remotepdsctl.Register(mgr, remotepdsctl.Options{Log: log, CABundle: caBundle, Secrets: secrets}); err != nil {
		log.Warn("the remote pds controller is not wired", "err", err.Error())
		return
	}
	if err := policyctl.Register(mgr, policyctl.Options{Bus: c.handlers, PLCURL: cfg.PLCBaseURL, Log: log, Disabled: cfg.DisablePolicy,
		Actions: func(workspace string) (map[string]policycontroller.ActionHandler, error) {
			return policyActionHandlers(c.store, workspace)
		}}); err != nil {
		log.Warn("the policy controllers are not wired", "err", err.Error())
	}
	if _, err := vaultctl.Register(mgr, c.handlers, vaultctl.Options{Log: log}); err != nil {
		log.Warn("the vault controller is not wired", "err", err.Error())
	}
	if run, err := eventsctl.Register(mgr, eventsctl.Options{Bus: c.handlers, Workspaces: cfg.Workspaces, Dynamic: eventstream.DynamicFactory(cfg.DynamicFactory),
		Resolver: &pdsclient.Resolver{Handlers: c.handlers, Timeout: pdsClientTimeout}, PLCURL: c.plcBaseURL(), StateDir: cfg.StateDir, Log: log}); err != nil {
		log.Warn("the event controllers are not wired", "err", err.Error())
	} else {
		c.runners = append(c.runners, controllerRunner{name: "events", run: run})
	}
	c.runners = append(c.runners, controllerRunner{name: "controller-manager", run: mgr.Start})
}

func (c *Core) plcBaseURL() string {
	return cmp.Or(strings.TrimSpace(c.cfg.PLCBaseURL),
		"http://"+kcp.ServiceFQDN(appctl.NetworkPLCName, "default", c.cfg.Workspaces[0], kcp.DefaultServiceDomain))
}

func (c *Core) managerRestConfig() (*rest.Config, error) {
	if c.cfg.Kubeconfig != "" {
		return kcpserver.FrontRestConfig(c.cfg.Kubeconfig)
	}
	if c.cfg.KCP == nil {
		return nil, ErrNoKubeconfig
	}
	client, err := c.cfg.KCP.Client(c.cfg.Workspaces[0])
	if err != nil {
		return nil, err
	}
	restCfg := client.RESTConfig()
	if restCfg == nil {
		return nil, errors.New("componentcore: the kcp client has no rest config")
	}
	return restCfg, nil
}

func (c *Core) mirrorLeases(ctx context.Context, ctl *leasecontroller.Controller) error {
	if ctl == nil {
		return errors.New("componentcore: the lease controller mirror has no controller")
	}
	log := c.cfg.Log.With("component", "lease-mirror")
	for ctx.Err() == nil {
		for _, workspace := range c.workspaceList(ctx) {
			workspace = strings.TrimSpace(workspace)
			if workspace == "" {
				continue
			}
			if err := ctl.Mirror(ctx, workspace); err != nil && ctx.Err() == nil {
				log.Warn("mirroring the allocator leases", "workspace", workspace, "err", err.Error())
			}
		}
		if !clock.Sleep(ctx, controllerResync) {
			return nil
		}
	}
	return nil
}

func (c *Core) startControllers(ctx context.Context) func() {
	if c.controllers == nil && len(c.runners) == 0 {
		return func() {}
	}
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	if c.controllers != nil {
		workers.Go(func() {
			if err := c.controllers.Run(runCtx); err != nil && runCtx.Err() == nil {
				c.cfg.Log.Warn("the core controllers stopped", "err", err.Error())
			}
		})
	}
	for _, runner := range c.runners {
		workers.Go(func() {
			if err := runner.run(runCtx); err != nil && runCtx.Err() == nil {
				c.cfg.Log.Warn("a core controller stopped", "controller", runner.name, "err", err.Error())
			}
		})
	}
	return func() {
		cancel()
		workers.Wait()
	}
}

func (c *Core) relayEndpoint(base string) statusstate.RelayEndpointFunc {
	return func(ref string) (string, bool) {
		ref = strings.TrimSpace(ref)
		ref = cmp.Or(ref, appctl.ComponentKindRelay+"/"+appctl.NetworkRelayName)
		if strings.Contains(ref, "://") {
			return ref, true
		}
		kind, name := appctl.ComponentKindRelay, ref
		if before, after, ok := strings.Cut(ref, "/"); ok {
			kind, name = before, after
		}
		kind, name = strings.TrimSpace(kind), strings.TrimSpace(name)
		kind = cmp.Or(kind, appctl.ComponentKindRelay)
		name = cmp.Or(name, appctl.NetworkRelayName)
		if endpoint, ok := c.registry.Endpoint(base, kind, name, "http"); ok {
			if endpoint.URL != "" {
				return endpoint.URL, true
			}
			if endpoint.Address != "" {
				return "http://" + endpoint.Address, true
			}
		}
		for _, url := range c.cfg.Relays {
			if url = strings.TrimSpace(url); url != "" {
				return url, true
			}
		}
		return "http://" + kcp.ServiceFQDN(strings.ToLower(kind), "default", base, kcp.DefaultServiceDomain), true
	}
}

func statusPDSResolver(resolver *pdsclient.Resolver, workspace string) statusstate.PDSResolver {
	return statusstate.PDSResolverFunc(func(_ context.Context, ref string) (pdsclient.PDS, error) {
		ref = strings.TrimSpace(ref)
		local := pdsclient.LocalRef{Workspace: workspace, Kind: appctl.ComponentKindPds}
		switch {
		case ref == "":
			return nil, errors.New("componentcore: the status object names no pdsRef")
		case strings.Contains(ref, "://"):
			local.Endpoint = ref
		case strings.Contains(ref, "/"):
			parts := strings.SplitN(ref, "/", 2)
			local.Kind = parts[0]
			local.Name = parts[1]
		default:
			local.Name = ref
		}
		return resolver.ResolveLocal(local)
	})
}

func coreSecretResolver(store *kcpstore.Store, workspace, client, server string) pdsclient.SecretResolver {
	if store == nil {
		return nil
	}
	resolver, err := credentials.NewKCPResolver(store, workspace, client, server)
	if err != nil {
		return nil
	}
	return pdsclient.SecretResolverFunc(func(ctx context.Context, raw string) (map[string]any, error) {
		ref, err := coreCredentialRef(raw)
		if err != nil {
			return nil, err
		}
		credential, err := resolver.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		return credential.Data, nil
	})
}

func coreCredentialRef(raw string) (component.CredentialRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return component.CredentialRef{}, errors.New("componentcore: the secret ref is empty")
	}
	if path, ok := strings.CutPrefix(raw, "file:"); ok {
		if path == "" {
			return component.CredentialRef{}, errors.New("componentcore: the file secret ref has no path")
		}
		return component.CredentialRef{Mount: "file", Path: path}, nil
	}
	raw = strings.TrimPrefix(raw, "openbao:")
	raw = strings.TrimPrefix(raw, "vault:")
	server, path, ok := strings.Cut(raw, ":")
	if !ok {
		server = ""
		path = raw
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return component.CredentialRef{}, errors.New("componentcore: the secret ref has no path")
	}
	return component.CredentialRef{Server: strings.TrimSpace(server), Path: path}, nil
}

func remotePDSSource(factory DynamicFactory) pdsclient.RemotePDSSource {
	if factory == nil {
		return nil
	}
	return func(ctx context.Context, workspace, name string) (*pdsv1alpha1.RemotePDS, error) {
		dyn, err := factory(workspace)
		if err != nil {
			return nil, fmt.Errorf("componentcore: the client for %s: %w", workspace, err)
		}
		if dyn == nil {
			return nil, fmt.Errorf("componentcore: the client for %s is nil", workspace)
		}
		u, err := dyn.Resource(remotepds.RemotePDSGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		var object pdsv1alpha1.RemotePDS
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &object); err != nil {
			return nil, fmt.Errorf("componentcore: decoding RemotePDS %s: %w", name, err)
		}
		return &object, nil
	}
}

func openBaoMetadata(store *kcpstore.Store, workspace string) statusstate.VaultSource {
	if store == nil {
		return nil
	}
	return &statusstate.OpenBaoMetadataSource{Open: func(ctx context.Context, server string) (statusstate.MetadataReader, error) {
		server = strings.TrimSpace(server)
		if server == "" {
			return nil, errors.New("componentcore: the vault status object names no OpenBaoServer")
		}
		obj, err := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, openbaoctl.GVR).Get(ctx, ref.New(workspace, "", server))
		if err != nil {
			return nil, fmt.Errorf("componentcore: reading OpenBaoServer %s: %w", server, err)
		}
		access, err := vaultmain.RootAccessOf(*obj)
		if err != nil {
			return nil, err
		}
		return access.Client(ctx)
	}}
}

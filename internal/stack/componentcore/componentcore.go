package componentcore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identitycontroller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyaction"
	policycontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/dispatchertls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/guestcontrolplane"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/plc"
	policyenginesvc "github.com/johnandersen777/socialweb-computer-kcp/internal/services/policyengine"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/requester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/sshserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsfront"
)

const (
	DefaultWorkspace   = "root"
	ComponentWorkspace = "app"
	RetryAfter         = 2 * time.Second
	PortsDir           = "ports"
)

var ErrNoKubeconfig = errors.New("componentcore: a kubeconfig or kcp clients are required")

type Config struct {
	Kubeconfig string

	Workspaces []string

	WorkspacesFunc func(ctx context.Context) []string

	StateDir string

	Log *slog.Logger

	TLSCertFile string

	TLSKeyFile string

	CredentialServer string

	CredentialClient string

	VaultClient string

	KCP component.KCPClients

	Store *kcpstore.Store

	DynamicFactory DynamicFactory

	Ports *component.PortRegistry

	Handlers *component.HandlerRegistry

	DisablePolicy bool

	ExposureIngressIdentity exposure.IngressIdentity

	ExposureIngressRegistrar exposure.IngressRegistrar

	SlotsFile string

	FrontControl string

	HostAuthority hostlifecycle.Authority

	InstallStateRoot string

	InstallAccount string

	PLCBaseURL string

	RelayURLs []string

	NodeClaimAgent *kcpclient.ServiceAccountRef

	OAuthScopes []string
}

type Core struct {
	cfg Config

	deps component.Deps

	runtime *component.Runtime

	controller *component.Controller

	registry *component.Registry

	ports *component.PortRegistry

	store *kcpstore.Store

	handlers *component.HandlerRegistry

	linkPolicy *exposure.LinkPolicy

	ingressRegistrar exposure.IngressRegistrar

	controllers *controllerRuntime

	runners []controllerRunner

	eventHandlers *eventstream.HandlerRegistry
}

func AppWorkspaces(base string) []string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = DefaultWorkspace
	}
	out := []string{base}
	app := base + ":" + ComponentWorkspace
	if app != base {
		out = append(out, app)
	}
	return out
}

func New(cfg Config) (*Core, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.StateDir == "" {
		return nil, errors.New("componentcore: a state dir is required")
	}
	if len(cfg.Workspaces) == 0 {
		cfg.Workspaces = AppWorkspaces(DefaultWorkspace)
	}
	if cfg.KCP == nil {
		set, err := component.NewKCPClientsFromKubeconfig(cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoKubeconfig, err)
		}
		cfg.KCP = set
	}
	base := cfg.Workspaces[0]
	store := cfg.Store
	if store == nil {
		client, err := cfg.KCP.Client(base)
		if err != nil {
			return nil, fmt.Errorf("componentcore: the kcp client for %s: %w", base, err)
		}
		restCfg := client.RESTConfig()
		if restCfg == nil {
			return nil, errors.New("componentcore: the kcp client has no rest config")
		}
		store, err = kcpstore.New(kcpstore.Options{Host: restCfg.Host, RestConfig: restCfg})
		if err != nil {
			return nil, fmt.Errorf("componentcore: the kcp store: %w", err)
		}
	}
	factory := cfg.DynamicFactory
	if factory == nil {
		if cfg.Kubeconfig != "" {
			f, err := NewDynamicFactory(cfg.Kubeconfig)
			if err != nil {
				return nil, err
			}
			factory = f
		} else {
			f, err := DynamicFromKCP(cfg.KCP)
			if err != nil {
				return nil, err
			}
			factory = f
		}
	}
	ports := cfg.Ports
	if ports == nil {
		var err error
		ports, err = component.OpenPortRegistry(component.PortRegistryOptions{StateDir: filepath.Join(cfg.StateDir, PortsDir)})
		if err != nil {
			return nil, fmt.Errorf("componentcore: the port registry: %w", err)
		}
	}
	cfg.DynamicFactory = factory
	if cfg.SlotsFile == "" {
		cfg.SlotsFile = filepath.Join(cfg.StateDir, "kcp", "kcp-slots.json")
	}
	if cfg.FrontControl == "" {
		cfg.FrontControl = filepath.Join(cfg.StateDir, "run", "front.sock")
	}
	tlsConfig, err := serverTLS(cfg)
	if err != nil {
		return nil, err
	}
	resolver, err := credentials.New(credentials.Options{Store: store, Workspace: base, Client: cfg.CredentialClient,
		Server: cfg.CredentialServer})
	if err != nil {
		return nil, fmt.Errorf("componentcore: the credential resolver: %w", err)
	}
	vault, err := component.NewOpenBaoVaultClients(store, base, cfg.VaultClient)
	if err != nil {
		return nil, fmt.Errorf("componentcore: the vault clients: %w", err)
	}
	registry := component.NewRegistry()
	handlers := cfg.Handlers
	if handlers == nil {
		handlers = component.NewHandlerRegistry()
	}
	deps := component.Deps{
		KCP:         cfg.KCP,
		Credentials: resolver,
		Vault:       vault,
		TLS:         tlsConfig,
		Log:         cfg.Log,
		StateDir:    cfg.StateDir,
		Hosts:       registryHosts{registry: registry},
		Registry:    registry,
		Handlers:    handlers,

		NodeClaimAgent: cfg.NodeClaimAgent,
	}
	registrar := cfg.ExposureIngressRegistrar
	fallbackName := "configured-registrar"
	if registrar == nil {
		registrar = exposure.NewSubscriberRegistrar(deps, cfg.ExposureIngressIdentity)
		fallbackName = "xrpc-subscription"
	}
	dispatcher := newIngressDispatcher(registry, registrar, fallbackName, cfg.Log)
	runtime := component.NewRuntime(deps, nil)
	actionHandlers, err := policyActionHandlers(store, base)
	if err != nil {
		return nil, err
	}
	policyOptions := policyenginesvc.Options{Store: store, Cluster: base, PLCURL: cfg.PLCBaseURL, Log: cfg.Log, Handlers: actionHandlers}
	if err := Register(runtime, policyOptions, cfg.ExposureIngressIdentity); err != nil {
		return nil, err
	}
	ctl, err := component.NewController(component.ControllerConfig{Runtime: runtime, Source: newSource(cfg, factory),
		Status: &statusWriter{dynamic: factory, log: cfg.Log}, Log: cfg.Log, Workers: 4})
	if err != nil {
		return nil, err
	}
	core := &Core{cfg: cfg, deps: deps, runtime: runtime, controller: ctl, registry: registry, ports: ports, store: store,
		handlers: handlers, linkPolicy: exposure.NewLinkPolicy(handlers), ingressRegistrar: dispatcher}
	core.buildControllers()
	return core, nil
}

func Register(runtime *component.Runtime, policyOptions policyenginesvc.Options, identity exposure.IngressIdentity) error {
	if err := ingressproxy.Register(runtime, identity); err != nil {
		return err
	}
	for _, register := range []func(*component.Runtime) error{plc.Register, relay.Register,
		dispatchertls.Register, gatewayapi.Register, bidder.Register, guestcontrolplane.Register, pds.Register,
		requester.Register, sshserver.Register} {
		if err := register(runtime); err != nil {
			return err
		}
	}
	return policyenginesvc.Register(runtime, policyOptions)
}

func policyActionHandlers(store *kcpstore.Store, workspace string) (map[string]policycontroller.ActionHandler, error) {
	handler, err := policyaction.NewComputeVMHandler(store, workspace)
	if err != nil {
		return nil, err
	}
	registry := &policycontroller.TriggerActionRegistry{}
	if err := registry.RegisterComputeVM(handler); err != nil {
		return nil, fmt.Errorf("componentcore: the ComputeVM policy action: %w", err)
	}
	return registry.Handlers(), nil
}

func (c *Core) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(component.WithSystemCaller(ctx))
	defer cancel()
	if !c.cfg.DisablePolicy {
		go c.runPolicy(runCtx)
	}
	go c.runExposure(runCtx)
	c.runIdentity(runCtx)
	stopControllers := c.startControllers(runCtx)
	defer stopControllers()
	return c.controller.Run(runCtx)
}

func (c *Core) Registry() *component.Registry {
	return c.registry
}

func (c *Core) Ports() *component.PortRegistry {
	return c.ports
}

func (c *Core) Runtime() *component.Runtime {
	return c.runtime
}

func (c *Core) Controller() *component.Controller {
	return c.controller
}

func (c *Core) Deps() component.Deps {
	return c.deps
}

func (c *Core) runIdentity(ctx context.Context) {
	log := c.cfg.Log.With("component", "identity-controller")
	seen := map[string]bool{}
	for _, workspace := range c.cfg.Workspaces {
		if workspace == "" || seen[workspace] {
			continue
		}
		seen[workspace] = true
		workspace := workspace
		var vault identitycontroller.SecretStore
		if store, err := newIdentityVaultStore(c.store, workspace, c.cfg.CredentialClient, c.cfg.CredentialServer); err == nil {
			vault = store
		}
		oauth := identitycontroller.OAuthOptions{Directory: identity.DefaultDirectory(), HTTPClient: &http.Client{Timeout: 20 * time.Second},
			States: &oauthclient.MemoryStateStore{}, Scopes: append([]string(nil), c.cfg.OAuthScopes...),
			QRScopes: atprotooauth.DIDKeyAssociatorOAuthScope}
		if key, err := loadOAuthCLIKey(c.cfg.StateDir); err == nil {
			oauth.CLIKey = key
			oauth.QROrigin = strings.TrimSpace(os.Getenv("SOCIALWEB_QR_ORIGIN"))
			if oauth.QROrigin == "" {
				oauth.QROrigin = "https://qr.fedfork.com"
			}
		}
		ctl, err := identitycontroller.New(identitycontroller.Options{Workspace: workspace, Dynamic: identitycontroller.DynamicFactory(c.cfg.DynamicFactory),
			Endpoints: c.pdsEndpoint, Resolver: &pdsclient.Resolver{Handlers: c.handlers, Timeout: 10 * time.Second},
			Vault: vault, OAuth: oauth, Log: log.With("workspace", workspace)})
		if err != nil {
			log.Warn("the MarketIdentity controller does not start", "workspace", workspace, "err", err.Error())
			continue
		}
		if len(c.cfg.Workspaces) > 0 && workspace == strings.TrimSpace(c.cfg.Workspaces[0]) {
			for _, name := range []string{"bidder", "requester"} {
				name := name
				oauthcallback.Register(name, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
					return ctl.CompleteBrowserAuthorization(ctx, name, r)
				})
			}
		}
		go func() {
			log.Info("the MarketIdentity controller writes its status in process", "workspace", workspace)
			if err := ctl.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("the MarketIdentity controller stopped", "workspace", workspace, "err", err.Error())
			}
		}()
	}
}

func (c *Core) pdsEndpoint(workspace, kind, name string) (string, bool) {
	if c.registry == nil {
		return "", false
	}
	if endpoint, ok := c.registry.Endpoint(workspace, kind, name, "http"); ok && endpoint.URL != "" {
		return endpoint.URL, true
	}
	endpoint, ok := component.PrimaryEndpoint(c.registry.Endpoints(workspace, kind, name))
	if !ok {
		return "", false
	}
	if endpoint.URL != "" {
		return endpoint.URL, true
	}
	if endpoint.Address != "" {
		return "http://" + endpoint.Address, true
	}
	return "", false
}

func (c *Core) runPolicy(ctx context.Context) {
	log := c.cfg.Log.With("component", "policy-controller")
	for _, workspace := range c.cfg.Workspaces {
		workspace := workspace
		go c.supervisePolicy(ctx, workspace, log.With("workspace", workspace))
	}
}

func (c *Core) supervisePolicy(ctx context.Context, workspace string, log *slog.Logger) {
	for ctx.Err() == nil {
		if !c.policyBound(ctx, workspace) {
			sleep(ctx, RetryAfter)
			continue
		}
		handlers, err := policyActionHandlers(c.store, workspace)
		if err != nil {
			log.Warn("the policy controller has no action handlers", "err", err.Error())
			sleep(ctx, RetryAfter)
			continue
		}
		svc, err := policyenginesvc.New(policyenginesvc.Options{Store: c.store, Cluster: workspace, PLCURL: c.cfg.PLCBaseURL, Log: log, Handlers: handlers})
		if err != nil {
			log.Warn("the policy controller does not start", "err", err.Error())
			sleep(ctx, RetryAfter)
			continue
		}
		log.Info("the policy controller reconciles PolicyEngine, PolicyWorkflow, PolicyWorkflowRun and PolicyTrigger in process")
		if err := svc.Controller().Run(ctx); ctx.Err() == nil {
			log.Warn("the policy controller stopped; starting it again", "err", fmt.Sprint(err))
		}
		sleep(ctx, RetryAfter)
	}
}

func (c *Core) policyBound(ctx context.Context, workspace string) bool {
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return false
	}
	_, err = dyn.Resource(appctl.PolicyEngineGVR).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return false
	}
	return false
}

func (c *Core) dynamic(workspace string) (dynamic.Interface, error) {
	return c.cfg.DynamicFactory(workspace)
}

func DynamicFromKCP(kcp component.KCPClients) (DynamicFactory, error) {
	if kcp == nil {
		return nil, ErrNoKubeconfig
	}
	return func(workspace string) (dynamic.Interface, error) {
		client, err := kcp.Client(workspace)
		if err != nil {
			return nil, err
		}
		return dynamicForRestConfig(client.RESTConfig(), workspace)
	}, nil
}

func NewDynamicFactory(kubeconfig string) (DynamicFactory, error) {
	if kubeconfig == "" {
		return nil, ErrNoKubeconfig
	}
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("componentcore: the kcp front rest config: %w", err)
	}
	return func(workspace string) (dynamic.Interface, error) {
		return dynamicForRestConfig(cfg, workspace)
	}, nil
}

func serverTLS(cfg Config) (*tls.Config, error) {
	certFile, keyFile := cfg.TLSCertFile, cfg.TLSKeyFile
	if certFile == "" && keyFile == "" {
		var err error
		if certFile, keyFile, err = tlsfront.EnsureSelfSigned(filepath.Join(cfg.StateDir, "tls")); err != nil {
			return nil, fmt.Errorf("componentcore: the self-signed front certificate: %w", err)
		}
	} else if certFile == "" || keyFile == "" {
		return nil, errors.New("componentcore: a TLS certificate and key are both required")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("componentcore: the TLS certificate and key: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

type registryHosts struct {
	registry *component.Registry
}

func (h registryHosts) Address(hostport string) (string, bool) {
	m, err := component.NewNetworkHostMapFromRegistry(h.registry)
	if err != nil {
		return "", false
	}
	return m.Address(hostport)
}

func (h registryHosts) Hosts() map[string]string {
	m, err := component.NewNetworkHostMapFromRegistry(h.registry)
	if err != nil {
		return nil
	}
	return m.Hosts()
}

func Supervise(ctx context.Context, cfg Config) {
	if ctx == nil {
		ctx = context.Background()
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "component-controller")
	for ctx.Err() == nil {
		core, err := New(cfg)
		if err == nil {
			log.Info("the component controller reconciles the application workspaces", "workspaces", strings.Join(cfg.Workspaces, ", "))
			err = core.Run(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Warn("the component controller stopped; starting it again", "err", fmt.Sprint(err))
		}
		sleep(ctx, RetryAfter)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func dynamicForRestConfig(cfg *rest.Config, workspace string) (dynamic.Interface, error) {
	if cfg == nil {
		return nil, errors.New("componentcore: the kcp client has no rest config")
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	out := rest.CopyConfig(cfg)
	host := strings.TrimSuffix(out.Host, "/")
	if !strings.Contains(host, "/clusters/") {
		out.Host = host + "/clusters/" + workspace
	}
	dc, err := dynamic.NewForConfig(out)
	if err != nil {
		return nil, fmt.Errorf("componentcore: the dynamic client for %s: %w", workspace, err)
	}
	return dc, nil
}

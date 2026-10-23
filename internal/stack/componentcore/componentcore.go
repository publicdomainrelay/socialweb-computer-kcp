package componentcore

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component/kcpclients"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyaction"
	policycontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/postgresctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/dispatchertls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/guestcontrolplane"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/plc"
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

	Relays []string

	NodeClaimAgent *kcpclient.ServiceAccountRef

	OAuthScopes []string

	Compute *computevm.Config

	Postgres *postgresctl.Host

	OpenBao *openbaoctl.Servers

	TTLScope func(workspace string) bool

	OnTTL func(report func() *v1alpha1.TTLStatus)

	Apps *app.Config
}

type Core struct {
	cfg Config

	credentials component.CredentialResolver

	tls *tls.Config

	hosts component.NetworkHosts

	runtime *component.Runtime

	scope *workspaceScope

	registry *component.Registry

	ports *component.PortRegistry

	store *kcpstore.Store

	handlers *component.HandlerRegistry

	linkPolicy *exposure.LinkPolicy

	ingressRegistrar exposure.IngressRegistrar

	closers []func()
}

func AppWorkspaces(base string) []string {
	base = strings.TrimSpace(base)
	base = cmp.Or(base, DefaultWorkspace)
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
		set, err := kcpclients.FromKubeconfig(cfg.Kubeconfig)
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
	tlsConfig, err := frontTLS(cfg)
	if err != nil {
		return nil, err
	}
	resolver, err := credentials.New(credentials.Options{Store: store, Workspace: base, Client: cfg.CredentialClient,
		Server: cfg.CredentialServer})
	if err != nil {
		return nil, fmt.Errorf("componentcore: the credential resolver: %w", err)
	}
	registry := component.NewRegistry()
	handlers := cfg.Handlers
	if handlers == nil {
		handlers = component.NewHandlerRegistry()
	}
	handlers.SetLogger(cfg.Log.With("component", "bus"))
	registrar := cfg.ExposureIngressRegistrar
	fallbackName := "configured-registrar"
	if registrar == nil {
		registrar = exposure.NewSubscriberRegistrar(registry, handlers, cfg.ExposureIngressIdentity)
		fallbackName = "xrpc-subscription"
	}
	dispatcher := newIngressDispatcher(registry, registrar, fallbackName, cfg.Log)
	runtime := component.NewRuntime(component.RuntimeConfig{Log: cfg.Log, StateDir: cfg.StateDir, Registry: registry, Bus: handlers}, nil)
	runtime.SetStateDirMaker(func(dir string) error { return os.MkdirAll(dir, 0o700) })
	core := &Core{cfg: cfg, credentials: resolver, tls: tlsConfig, hosts: registryHosts{registry: registry}, runtime: runtime,
		registry: registry, ports: ports, store: store, handlers: handlers, linkPolicy: exposure.NewLinkPolicy(handlers),
		ingressRegistrar: dispatcher, scope: newWorkspaceScope(cfg.Workspaces, cfg.WorkspacesFunc)}
	if err := core.register(); err != nil {
		return nil, err
	}
	return core, nil
}

func (c *Core) register() error {
	c.provideComponents()
	kcp, creds, tlsConfig, hosts, registry, identity := c.cfg.KCP, c.credentials, c.tls, c.hosts, c.registry, c.cfg.ExposureIngressIdentity
	egress := func(in component.Instance) component.Egress {
		return component.Egress{Hosts: hosts, TLS: tlsConfig, Log: in.Log}
	}
	for _, entry := range []struct {
		kind    string
		factory component.ServiceFactory
	}{
		{plc.Kind, component.Typed(func(in component.Instance, spec v1alpha1.PlcSpec) (*plc.Service, error) {
			return plc.New(spec, in), nil
		})},
		{relay.Kind, component.Typed(func(in component.Instance, spec v1alpha1.RelaySpec) (*relay.Service, error) {
			return relay.New(spec, relay.Deps{Instance: in, Registry: registry, TLS: tlsConfig, Egress: egress(in)})
		})},
		{dispatchertls.Kind, component.Typed(func(in component.Instance, spec v1alpha1.DispatcherTLSSpec) (*dispatchertls.Service, error) {
			return dispatchertls.New(spec, dispatchertls.Deps{Instance: in, Registry: registry, TLS: tlsConfig}), nil
		})},
		{ingressproxy.Kind, component.Typed(func(in component.Instance, spec v1alpha1.IngressProxySpec) (*ingressproxy.Service, error) {
			return ingressproxy.New(spec, ingressproxy.Deps{Instance: in, Registry: registry, TLS: tlsConfig, Identity: identity})
		})},
		{gatewayapi.Kind, component.Typed(func(in component.Instance, spec v1alpha1.GatewayAPISpec) (*gatewayapi.Service, error) {
			return gatewayapi.New(spec, gatewayapi.Deps{Instance: in, KCP: kcp, TLS: tlsConfig, NodeClaimAgent: c.cfg.NodeClaimAgent})
		})},
		{bidder.Kind, component.Typed(func(in component.Instance, spec v1alpha1.BidderSpec) (*bidder.Service, error) {
			return bidder.New(spec, bidder.Deps{Instance: in, KCP: kcp, Credentials: creds, Egress: egress(in)})
		})},
		{guestcontrolplane.Kind, component.Typed(func(in component.Instance, spec v1alpha1.GuestControlPlaneSpec) (*guestcontrolplane.Service, error) {
			return guestcontrolplane.New(spec, guestcontrolplane.Deps{Instance: in, Hosts: hosts})
		})},
		{pds.Kind, component.Typed(func(in component.Instance, spec v1alpha1.PdsSpec) (*pds.Service, error) {
			return pds.New(spec, pds.Deps{Instance: in, Egress: egress(in)})
		})},
		{requester.Kind, component.Typed(func(in component.Instance, spec v1alpha1.RequesterSpec) (*requester.Service, error) {
			return requester.New(spec, requester.Deps{Instance: in, KCP: kcp, Credentials: creds, Egress: egress(in)})
		})},
		{sshserver.Kind, component.Typed(func(in component.Instance, spec v1alpha1.SshServerSpec) (*sshserver.Service, error) {
			return sshserver.New(spec, sshserver.Deps{Instance: in, Hosts: hosts, TLS: tlsConfig})
		})},
	} {
		if err := c.runtime.Register(entry.kind, entry.factory); err != nil {
			return err
		}
	}
	return nil
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
	if err := c.runtime.Start(ctx); err != nil {
		return err
	}
	err := c.runControllers(ctx)
	for _, closer := range c.closers {
		closer()
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), component.DefaultStopTimeout)
	defer cancel()
	return errors.Join(err, c.runtime.Stop(stopCtx))
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

func frontTLS(cfg Config) (*tls.Config, error) {
	certFile, keyFile := cfg.TLSCertFile, cfg.TLSKeyFile
	if certFile == "" && keyFile == "" {
		var err error
		if certFile, keyFile, err = tlsfront.EnsureSelfSigned(filepath.Join(cfg.StateDir, "tls")); err != nil {
			return nil, fmt.Errorf("componentcore: the self-signed front certificate: %w", err)
		}
	} else if certFile == "" || keyFile == "" {
		return nil, errors.New("componentcore: a TLS certificate and key are both required")
	}
	reloader, err := tlsreload.New(certFile, []string{keyFile}, cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("componentcore: the TLS certificate and key: %w", err)
	}
	return reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}), nil
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
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "component-controller")
	supervise.Restart(ctx, "componentcore", supervise.Backoff{Min: RetryAfter, Max: RetryAfter, OnError: func(err error, _ time.Duration) {
		if err != nil {
			log.Warn("the component controller stopped; starting it again", "err", fmt.Sprint(err))
		}
	}}, func(ctx context.Context) error {
		core, err := New(cfg)
		if err != nil {
			return err
		}
		log.Info("the component controller reconciles the application workspaces", "workspaces", strings.Join(cfg.Workspaces, ", "))
		return core.Run(ctx)
	})
}

func dynamicForRestConfig(cfg *rest.Config, workspace string) (dynamic.Interface, error) {
	if cfg == nil {
		return nil, errors.New("componentcore: the kcp client has no rest config")
	}
	workspace = strings.TrimSpace(workspace)
	workspace = cmp.Or(workspace, DefaultWorkspace)
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

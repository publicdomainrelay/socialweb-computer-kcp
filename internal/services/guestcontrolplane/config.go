package guestcontrolplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

const Kind = "GuestControlPlane"

const (
	DefaultServeHost = "127.0.0.1"

	DefaultGuestLoginMount = "sshsig"

	DefaultVaultServer = "bidder"

	DefaultVaultClient = "guest-kcp"

	DefaultKVClient = "bidder"

	DefaultRole = "kcp"

	DefaultGrantSync = 30 * time.Second

	DefaultIssuerRetry = 30 * time.Second

	DefaultOpenBaoTimeout = 10 * time.Second
)

type RelayConfig struct {
	IngressProxyHost string

	Label string

	Keypair RelayKeypair

	GetServiceAuthToken func(ctx context.Context, nsid string) (string, error)

	TLS bool
}

type RelayKeypair interface {
	DID() string

	Sign(data []byte) ([]byte, error)
}

type Config struct {
	Deps component.Deps

	StateDir string

	Workspace string

	Namespace string

	ComponentName string

	Network v1alpha1.MarketNetworkValue

	Identity v1alpha1.ComponentIdentity

	IdentityRef *v1alpha1.ComponentRef

	PDSRef *v1alpha1.ComponentRef

	VaultRef *v1alpha1.ComponentRef

	HostKeyRef string

	Handle string

	DID string

	VaultServer string

	VaultClient string

	KVClient string

	GuestLoginMount string

	Role string

	ServeHost string

	ServePort int

	Issuer string

	Relay *RelayConfig

	OpenBao OpenBaoSource

	Endpoint func(ctx context.Context) (guestidentity.Endpoint, error)

	VMs VMStore

	Grants GuestStore

	RBAC guestidentity.RBACSource

	GrantSync time.Duration

	HostKeyAttempts int

	HostKeyRetry time.Duration

	IssuerRetry time.Duration

	Now func() time.Time
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var guestSpec v1alpha1.GuestControlPlaneSpec
	if err := spec.Spec.Decode(&guestSpec); err != nil {
		return Config{}, err
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	cfg := Config{
		Deps:            deps,
		StateDir:        stateDir,
		Workspace:       spec.Workspace,
		Namespace:       spec.Namespace,
		ComponentName:   spec.Name,
		IdentityRef:     guestSpec.PdsRef,
		PDSRef:          guestSpec.PdsRef,
		VaultRef:        guestSpec.VaultRef,
		HostKeyRef:      guestSpec.HostKeyRef,
		VaultServer:     DefaultVaultServer,
		VaultClient:     DefaultVaultClient,
		KVClient:        DefaultKVClient,
		GuestLoginMount: DefaultGuestLoginMount,
		Role:            DefaultRole,
		ServeHost:       DefaultServeHost,
		GrantSync:       DefaultGrantSync,
		IssuerRetry:     DefaultIssuerRetry,
	}
	if guestSpec.Identity != nil {
		cfg.Identity = *guestSpec.Identity
		cfg.IdentityRef = guestSpec.Identity.MarketIdentityRef
		cfg.Handle = guestSpec.Identity.Handle
		cfg.DID = guestSpec.Identity.Did
	}
	if guestSpec.Network != nil {
		cfg.Network = *guestSpec.Network
	}
	if guestSpec.Serve != nil {
		if guestSpec.Serve.Host != "" {
			cfg.ServeHost = guestSpec.Serve.Host
		}
		cfg.ServePort = int(guestSpec.Serve.Port)
	}
	if guestSpec.VaultRef != nil && guestSpec.VaultRef.Name != "" {
		cfg.VaultServer = guestSpec.VaultRef.Name
	}
	if guestSpec.Storage != nil && guestSpec.Storage.DataDir != "" {
		cfg.StateDir = guestSpec.Storage.DataDir
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "root"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg, nil
}

func (c Config) check() error {
	switch {
	case c.Handle == "":
		return errors.New("guestcontrolplane: the component names no bidder handle")
	case c.VaultServer == "":
		return errors.New("guestcontrolplane: the component names no OpenBaoServer")
	case c.VaultClient == "":
		return errors.New("guestcontrolplane: the component names no OpenBao client")
	case !guestidentity.ValidMount(c.GuestLoginMount):
		return fmt.Errorf("guestcontrolplane: %q is not an auth mount name", c.GuestLoginMount)
	case c.Endpoint == nil && c.OpenBao == nil && c.Deps.KCP == nil:
		return errors.New("guestcontrolplane: the component has no OpenBao endpoint source")
	}
	return nil
}

func (c *Config) defaults() {
	if c.ServeHost == "" {
		c.ServeHost = DefaultServeHost
	}
	if c.GuestLoginMount == "" {
		c.GuestLoginMount = DefaultGuestLoginMount
	}
	if c.VaultServer == "" {
		c.VaultServer = DefaultVaultServer
	}
	if c.VaultClient == "" {
		c.VaultClient = DefaultVaultClient
	}
	if c.KVClient == "" {
		c.KVClient = DefaultKVClient
	}
	if c.Role == "" {
		c.Role = DefaultRole
	}
	if c.GrantSync <= 0 {
		c.GrantSync = DefaultGrantSync
	}
	if c.IssuerRetry <= 0 {
		c.IssuerRetry = DefaultIssuerRetry
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Workspace == "" {
		c.Workspace = "root"
	}
	if c.Issuer != "" {
		c.Issuer = strings.TrimRight(c.Issuer, "/")
	}
	if c.StateDir == "" {
		c.StateDir = c.Deps.StateDir
	}
}

func Factory() component.ServiceFactory {
	return func(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
		cfg, err := ConfigFrom(deps, spec)
		if err != nil {
			return nil, err
		}
		return New(cfg)
	}
}

func Register(runtime *component.Runtime) error {
	return runtime.Register(Kind, Factory())
}

func New(cfg Config) (*Service, error) {
	cfg.defaults()
	if err := cfg.check(); err != nil {
		return nil, err
	}
	log := cfg.Deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg, log: log.With("component", "guestcontrolplane", "handle", cfg.Handle)}, nil
}

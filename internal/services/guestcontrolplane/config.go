package guestcontrolplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
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

	IdentityRef *commonv1.ObjectRef

	PDSRef *commonv1.ObjectRef

	VaultRef *commonv1.ObjectRef

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
	if guestSpec.GuestLoginMount != "" {
		cfg.GuestLoginMount = guestSpec.GuestLoginMount
	}
	if guestSpec.Role != "" {
		cfg.Role = guestSpec.Role
	}
	if guestSpec.GrantSyncSeconds != nil && *guestSpec.GrantSyncSeconds > 0 {
		cfg.GrantSync = time.Duration(*guestSpec.GrantSyncSeconds) * time.Second
	}
	if guestSpec.IssuerRetrySeconds != nil && *guestSpec.IssuerRetrySeconds > 0 {
		cfg.IssuerRetry = time.Duration(*guestSpec.IssuerRetrySeconds) * time.Second
	}
	if guestSpec.HostKeyAttempts != nil && *guestSpec.HostKeyAttempts > 0 {
		cfg.HostKeyAttempts = int(*guestSpec.HostKeyAttempts)
	}
	if guestSpec.RelayEnabled != nil && *guestSpec.RelayEnabled {
		cfg.Relay = &RelayConfig{IngressProxyHost: strings.TrimSpace(cfg.Network.IngressProxyHost),
			TLS: strings.HasPrefix(strings.ToLower(strings.TrimSpace(cfg.Network.GuestTLSURL())), "https://")}
	}
	cfg.VaultServer = v1alpha1.VaultServer(guestSpec.VaultRef, cfg.VaultServer)
	if guestSpec.Storage != nil && guestSpec.Storage.DataDir != "" {
		cfg.StateDir = guestSpec.Storage.DataDir
	}
	cfg.Workspace = cmp.Or(cfg.Workspace, "root")
	cfg.Namespace = cmp.Or(cfg.Namespace, "default")
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
	c.ServeHost = cmp.Or(c.ServeHost, DefaultServeHost)
	c.GuestLoginMount = cmp.Or(c.GuestLoginMount, DefaultGuestLoginMount)
	c.VaultServer = cmp.Or(c.VaultServer, DefaultVaultServer)
	c.VaultClient = cmp.Or(c.VaultClient, DefaultVaultClient)
	c.KVClient = cmp.Or(c.KVClient, DefaultKVClient)
	c.Role = cmp.Or(c.Role, DefaultRole)
	c.GrantSync = opt.Positive(c.GrantSync, DefaultGrantSync)
	c.IssuerRetry = opt.Positive(c.IssuerRetry, DefaultIssuerRetry)
	if c.Now == nil {
		c.Now = time.Now
	}
	c.Workspace = cmp.Or(c.Workspace, "root")
	if c.Issuer != "" {
		c.Issuer = strings.TrimRight(c.Issuer, "/")
	}
	c.StateDir = cmp.Or(c.StateDir, c.Deps.StateDir)
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

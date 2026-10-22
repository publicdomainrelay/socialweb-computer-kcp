package bidder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
)

const Kind = "Bidder"

const (
	DefaultServeHost = "127.0.0.1"

	DefaultGuestLoginMount = computevm.GuestLoginDefaultMount

	DefaultGuestVaultServer = computevm.VaultServer

	DefaultGuestVaultMount = computevm.VaultMount

	DefaultGuestControlName = "guest-kcp"

	DefaultBidTTL = 24 * time.Hour

	DefaultPolicyTimeout = 2 * time.Minute

	DefaultPolicyPoll = 250 * time.Millisecond

	DefaultBootTimeout = 2 * time.Minute

	DefaultReleaseTimeout = 150 * time.Second

	DefaultOfferingRefresh = 5 * time.Minute
)

type RelayConfig struct {
	IngressProxyHost string

	ConnectHost string

	DialAddress string

	PublicURLBase string

	Label string

	Keypair RelayKeypair

	GetServiceAuthToken func(ctx context.Context, nsid string) (string, error)

	HTTPClient *http.Client

	TLS bool
}

type RelayKeypair interface {
	DID() string

	Sign(data []byte) ([]byte, error)
}

type ProviderConfig struct {
	Handle string

	GuestLoginMount string

	GuestHostAddress string

	GuestTLSURL string

	GuestTLSPort int

	GuestVaultServer string

	GuestVaultMount string

	AcceptPath string

	ReuseStale bool

	StopGraceSeconds int32

	BootTimeout time.Duration

	ReleaseTimeout time.Duration

	MaxConcurrentGuests int32
}

type ServiceCall func(ctx context.Context, endpoint, nsid, lxm string, body map[string]any) error

type ServiceAuthConfig struct {
	Hostname string

	AudienceDIDs []string
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

	PolicyEngineRef *v1alpha1.ComponentRef

	GuestControlRef *v1alpha1.ComponentRef

	Handle string

	DID string

	PDSURL string

	PDSAccessToken string

	PDSAccessTokenRef *component.CredentialRef

	PDS pdsclient.PDS

	PDSResolver *pdsclient.Resolver

	PDSTokens pdsclient.TokenSource

	PDSDPoP pdsclient.DPoPSource

	RemotePDS *pdsv1alpha1.RemotePDS

	VaultClient string

	RepoDID string

	Policy string

	PolicyArgs map[string]string

	BidderDIDs []string

	DenyBidderDIDs []string

	PolicyTimeout time.Duration

	PolicyPoll time.Duration

	Provider ProviderConfig

	ServiceAuth ServiceAuthConfig

	ServeHost string

	ServePort int

	Relay *RelayConfig

	Records RecordStore

	PolicyRuns PolicyRunClient

	Vault GuestVault

	Bids BidStore

	VMs VMStore

	Grants GrantStore

	GuestURL string

	Authorize func(ctx context.Context, header, lxm, recordURI string) (string, error)

	CallService ServiceCall

	Now func() time.Time
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var bidderSpec v1alpha1.BidderSpec
	if err := spec.Spec.Decode(&bidderSpec); err != nil {
		return Config{}, err
	}
	policyArgs, err := parsePolicyArgs(bidderSpec.PolicyArgs)
	if err != nil {
		return Config{}, err
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	cfg := Config{
		Deps:              deps,
		StateDir:          stateDir,
		Workspace:         spec.Workspace,
		Namespace:         spec.Namespace,
		ComponentName:     spec.Name,
		IdentityRef:       bidderSpec.PdsRef,
		PDSRef:            bidderSpec.PdsRef,
		VaultRef:          bidderSpec.VaultRef,
		PDSAccessTokenRef: accessTokenRef(bidderSpec.PDSAccessTokenPath, bidderSpec.VaultRef),
		PolicyEngineRef:   bidderSpec.PolicyEngineRef,
		GuestControlRef:   &v1alpha1.ComponentRef{Kind: "GuestControlPlane", Name: DefaultGuestControlName},
		ServeHost:         DefaultServeHost,
		Policy:            bidderSpec.Policy,
		PolicyArgs:        policyArgs,
		BidderDIDs:        append([]string(nil), bidderSpec.BidderDIDs...),
		PolicyTimeout:     DefaultPolicyTimeout,
		PolicyPoll:        DefaultPolicyPoll,
		VaultClient:       "bidder",
	}
	if bidderSpec.Identity != nil {
		cfg.Identity = *bidderSpec.Identity
		cfg.IdentityRef = bidderSpec.Identity.MarketIdentityRef
		cfg.Handle = bidderSpec.Identity.Handle
		cfg.DID = bidderSpec.Identity.Did
	}
	if bidderSpec.Network != nil {
		cfg.Network = *bidderSpec.Network
		if hosts := bidderNetworkResolver(cfg.Network, cfg.Deps.Hosts); hosts != nil {
			cfg.Deps.Hosts = hosts
		}
		if strings.TrimSpace(cfg.Network.CaBundleRef) != "" {
			tlsConfig, err := bidderNetworkTLS(cfg.Network.CaBundleRef)
			if err != nil {
				return Config{}, err
			}
			cfg.Deps.TLS = tlsConfig
			if deps.Log != nil {
				deps.Log.Info("the bidder trusts the network CA", "bundle", cfg.Network.CaBundleRef)
			}
		}
		if cfg.Network.IngressProxyHost != "" {
			cfg.Relay = &RelayConfig{
				IngressProxyHost: cfg.Network.IngressProxyHost,
				ConnectHost:      cfg.Network.IngressProxyHost,
				DialAddress:      ingressConnectHost(cfg.Network),
				PublicURLBase:    cfg.Network.GuestTlsUrl,
				Label:            "market",
			}
		}
	}
	if bidderSpec.Serve != nil {
		if bidderSpec.Serve.Host != "" {
			cfg.ServeHost = bidderSpec.Serve.Host
		}
		cfg.ServePort = int(bidderSpec.Serve.Port)
	}
	cfg.Provider = ProviderConfig{
		Handle:           cfg.Handle,
		GuestLoginMount:  DefaultGuestLoginMount,
		GuestHostAddress: v1alpha1.GuestHostAddress,
		GuestVaultServer: DefaultGuestVaultServer,
		GuestVaultMount:  DefaultGuestVaultMount,
		AcceptPath:       guestseed.AcceptPathVM,
		BootTimeout:      DefaultBootTimeout,
		ReleaseTimeout:   DefaultReleaseTimeout,
	}
	if bidderSpec.Kcp != nil {
		if bidderSpec.Kcp.Handle != "" {
			cfg.Provider.Handle = bidderSpec.Kcp.Handle
			if cfg.Handle == "" {
				cfg.Handle = bidderSpec.Kcp.Handle
			}
		}
		cfg.Provider.StopGraceSeconds = bidderSpec.Kcp.StopGraceSeconds
	}
	cfg.Provider.GuestVaultServer = bidderSpec.VaultRef.VaultServer(cfg.Provider.GuestVaultServer)
	if bidderSpec.Storage != nil && bidderSpec.Storage.DataDir != "" {
		cfg.StateDir = bidderSpec.Storage.DataDir
	}
	if bidderSpec.MaxConcurrentGuests > 0 {
		cfg.Provider.MaxConcurrentGuests = bidderSpec.MaxConcurrentGuests
	}
	cfg.Provider.GuestTLSURL = cfg.Network.GuestTlsUrl
	if port := portOf(cfg.Network.GuestTlsUrl); port > 0 {
		cfg.Provider.GuestTLSPort = port
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

type bidderChainedResolver []networkdial.Resolver

func (c bidderChainedResolver) Address(hostport string) (string, bool) {
	for _, resolver := range c {
		if resolver == nil {
			continue
		}
		if address, ok := resolver.Address(hostport); ok {
			return address, true
		}
	}
	return "", false
}

func (c bidderChainedResolver) Hosts() map[string]string {
	out := map[string]string{}
	for _, resolver := range c {
		source, ok := resolver.(interface{ Hosts() map[string]string })
		if !ok {
			continue
		}
		for name, address := range source.Hosts() {
			if _, exists := out[name]; !exists {
				out[name] = address
			}
		}
	}
	return out
}

func bidderNetworkResolver(n v1alpha1.MarketNetworkValue, fallback networkdial.Resolver) component.NetworkHosts {
	entries := make([]networkdial.Host, 0, len(n.Hosts)+1)
	for _, host := range n.Hosts {
		entries = append(entries, networkdial.Host{Name: host.Name, Address: host.Address})
	}
	if name := hostnameOnly(n.GuestTlsUrl); name != "" {
		if address := bidderHostWithPort(n.GuestTlsUrl, n.Hosts); address != "" {
			entries = append(entries, networkdial.Host{Name: name, Address: address})
		}
	}
	resolvers := make([]networkdial.Resolver, 0, 2)
	if mapped, err := networkdial.NewHostMap(entries); err == nil && mapped.Len() > 0 {
		resolvers = append(resolvers, mapped)
	}
	if fallback != nil {
		resolvers = append(resolvers, fallback)
	}
	if len(resolvers) == 0 {
		return nil
	}
	return bidderChainedResolver(resolvers)
}

func bidderHostWithPort(raw string, hosts []v1alpha1.NetworkHost) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	if port == "" {
		return ""
	}
	for _, host := range hosts {
		if _, candidate, splitErr := net.SplitHostPort(host.Address); splitErr == nil && candidate == port {
			return host.Address
		}
	}
	return ""
}

func bidderNetworkTLS(path string) (*tls.Config, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("bidder: the network CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("bidder: the network CA bundle %s has no certificates", path)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

func ingressConnectHost(n v1alpha1.MarketNetworkValue) string {
	_, port, err := net.SplitHostPort(n.IngressProxyHost)
	if err != nil || port == "" {
		return n.IngressProxyHost
	}
	for _, host := range n.Hosts {
		if _, candidate, splitErr := net.SplitHostPort(host.Address); splitErr == nil && candidate == port {
			return host.Address
		}
	}
	return n.IngressProxyHost
}

func accessTokenRef(raw string, vaultRef *v1alpha1.ComponentRef) *component.CredentialRef {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	ref := &component.CredentialRef{Keys: []string{"accessJwt", "accessToken", "access_token", "token", "value"}}
	switch {
	case strings.HasPrefix(raw, "openbao:"), strings.HasPrefix(raw, "vault:"):
		raw = strings.TrimPrefix(strings.TrimPrefix(raw, "openbao:"), "vault:")
		ref.Path = strings.Trim(raw, "/")
		if vaultRef != nil {
			ref.Server = strings.TrimSpace(vaultRef.Name)
		}
	case strings.HasPrefix(raw, "file:"):
		ref.Server = "file:"
		ref.Path = strings.TrimPrefix(raw, "file:")
	default:
		ref.Mount = "file"
		ref.Path = raw
	}
	return ref
}

func parsePolicyArgs(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("bidder: policyArgs is not JSON: %w", err)
	}
	if value == nil {
		return map[string]string{}, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("bidder: policyArgs must be a JSON object")
	}
	out := make(map[string]string, len(obj))
	for key, item := range obj {
		switch typed := item.(type) {
		case string:
			out[key] = typed
		case bool:
			out[key] = fmt.Sprintf("%t", typed)
		case float64:
			out[key] = fmt.Sprintf("%v", typed)
		default:
			data, err := json.Marshal(typed)
			if err != nil {
				return nil, err
			}
			out[key] = string(data)
		}
	}
	return out, nil
}

func portOf(raw string) int {
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return 0
	}
	port := 0
	for _, r := range u.Port() {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	return port
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (c Config) check() error {
	switch {
	case c.Handle == "":
		return errors.New("bidder: the component names no handle")
	case c.Provider.GuestLoginMount == "":
		return errors.New("bidder: the provider names no guest login mount")
	case c.Provider.GuestVaultServer == "":
		return errors.New("bidder: the provider names no guest vault server")
	case strings.Trim(c.Provider.GuestVaultMount, "/") == "":
		return errors.New("bidder: the provider names no guest vault mount")
	}
	return nil
}

func (c Config) withDeps(deps component.Deps) Config {
	out := c
	if out.Deps.Log == nil {
		out.Deps.Log = deps.Log
	}
	if out.Deps.Registry == nil {
		out.Deps.Registry = deps.Registry
	}
	if out.Deps.KCP == nil {
		out.Deps.KCP = deps.KCP
	}
	if out.Deps.Credentials == nil {
		out.Deps.Credentials = deps.Credentials
	}
	if out.Deps.Vault == nil {
		out.Deps.Vault = deps.Vault
	}
	if out.Deps.Hosts == nil {
		out.Deps.Hosts = deps.Hosts
	}
	if out.Deps.TLS == nil {
		out.Deps.TLS = deps.TLS
	}
	if out.Deps.Handlers == nil {
		out.Deps.Handlers = deps.Handlers
	}
	if out.Deps.StateDir == "" {
		out.Deps.StateDir = deps.StateDir
	}
	if out.StateDir == "" {
		out.StateDir = out.Deps.StateDir
	}
	return out
}

func (c *Config) defaults() {
	if c.ServeHost == "" {
		c.ServeHost = DefaultServeHost
	}
	if c.PolicyTimeout <= 0 {
		c.PolicyTimeout = DefaultPolicyTimeout
	}
	if c.PolicyPoll <= 0 {
		c.PolicyPoll = DefaultPolicyPoll
	}
	if c.Provider.GuestLoginMount == "" {
		c.Provider.GuestLoginMount = DefaultGuestLoginMount
	}
	if c.Provider.GuestHostAddress == "" {
		c.Provider.GuestHostAddress = v1alpha1.GuestHostAddress
	}
	if c.Provider.GuestVaultServer == "" {
		c.Provider.GuestVaultServer = DefaultGuestVaultServer
	}
	if c.Provider.GuestVaultMount == "" {
		c.Provider.GuestVaultMount = DefaultGuestVaultMount
	}
	if c.Provider.AcceptPath == "" {
		c.Provider.AcceptPath = guestseed.AcceptPathVM
	}
	if c.Provider.BootTimeout <= 0 {
		c.Provider.BootTimeout = DefaultBootTimeout
	}
	if c.Provider.ReleaseTimeout <= 0 {
		c.Provider.ReleaseTimeout = DefaultReleaseTimeout
	}
	if c.Provider.Handle == "" {
		c.Provider.Handle = c.Handle
	}
	if c.VaultClient == "" {
		c.VaultClient = "bidder"
	}
	if c.GuestControlRef == nil {
		c.GuestControlRef = &v1alpha1.ComponentRef{Kind: "GuestControlPlane", Name: DefaultGuestControlName}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.PolicyArgs == nil {
		c.PolicyArgs = map[string]string{}
	}
	if c.Workspace == "" {
		c.Workspace = "root"
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
	return &Service{cfg: cfg, log: log.With("component", "bidder", "handle", cfg.Handle)}, nil
}

func handlerURL(workspace, kind, name string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	name = strings.TrimSpace(name)
	if kind == "" || name == "" {
		return ""
	}
	return "http://" + kcp.ServiceFQDN(kind, "default", workspace, kcp.DefaultServiceDomain)
}

func externalHTTPClient(deps component.Deps, timeout time.Duration) *http.Client {
	transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{
		Hosts:          deps.Hosts,
		TLSConfig:      deps.TLS,
		AllowSystemDNS: true,
	})
	if err != nil {
		if deps.Log != nil {
			deps.Log.Warn("the bidder's external HTTP transport is not configured", "err", err)
		}
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func isAbsoluteURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

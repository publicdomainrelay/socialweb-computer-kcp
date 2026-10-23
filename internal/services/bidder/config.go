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
	"strings"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
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

	IdentityRef *commonv1.ObjectRef

	PDSRef *commonv1.ObjectRef

	VaultRef *commonv1.ObjectRef

	PolicyEngineRef *commonv1.ObjectRef

	GuestControlRef *commonv1.ObjectRef

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

	MarketURL string

	MarketExposureRef *commonv1.LocalObjectRef

	IdentityMode string

	DenyBidderDIDs []string

	PolicyTimeout time.Duration

	PolicyPoll time.Duration

	Heartbeat time.Duration

	OfferingRefresh time.Duration

	OfferingRefreshDisabled bool

	BidTTL time.Duration

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

	Comms *v1alpha1.MarketComms

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
	if err := validateMarketComms("bidder", bidderSpec.Comms); err != nil {
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
		PDSAccessTokenRef: accessTokenRef(bidderSpec.PDSAccessToken, bidderSpec.VaultRef),
		PolicyEngineRef:   bidderSpec.PolicyEngineRef,
		GuestControlRef:   &commonv1.ObjectRef{Kind: "GuestControlPlane", Name: DefaultGuestControlName},
		ServeHost:         DefaultServeHost,
		Policy:            bidderSpec.Policy,
		PolicyArgs:        policyArgs,
		BidderDIDs:        append([]string(nil), bidderSpec.BidderDIDs...),
		MarketExposureRef: bidderSpec.MarketExposureRef,
		IdentityMode:      strings.TrimSpace(bidderSpec.Mode),
		Comms:             bidderSpec.Comms,
		PolicyTimeout:     DefaultPolicyTimeout,
		PolicyPoll:        DefaultPolicyPoll,
		OfferingRefresh:   DefaultOfferingRefresh,
		BidTTL:            DefaultBidTTL,
		VaultClient:       "bidder",
	}
	if bidderSpec.HeartbeatIntervalSeconds != nil && *bidderSpec.HeartbeatIntervalSeconds > 0 {
		cfg.Heartbeat = time.Duration(*bidderSpec.HeartbeatIntervalSeconds) * time.Second
	}
	if bidderSpec.OfferingRefreshSeconds != nil {
		if *bidderSpec.OfferingRefreshSeconds == 0 {
			cfg.OfferingRefreshDisabled = true
		} else {
			cfg.OfferingRefresh = time.Duration(*bidderSpec.OfferingRefreshSeconds) * time.Second
		}
	}
	if bidderSpec.BidTTLSeconds != nil && *bidderSpec.BidTTLSeconds > 0 {
		cfg.BidTTL = time.Duration(*bidderSpec.BidTTLSeconds) * time.Second
	}
	if bidderSpec.PolicyPollMilliseconds != nil && *bidderSpec.PolicyPollMilliseconds > 0 {
		cfg.PolicyPoll = time.Duration(*bidderSpec.PolicyPollMilliseconds) * time.Millisecond
	}
	if bidderSpec.PolicyTimeoutSeconds != nil && *bidderSpec.PolicyTimeoutSeconds > 0 {
		cfg.PolicyTimeout = time.Duration(*bidderSpec.PolicyTimeoutSeconds) * time.Second
	}
	if bidderSpec.ServiceAuth != nil {
		cfg.ServiceAuth.Hostname = strings.TrimSpace(bidderSpec.ServiceAuth.Hostname)
		cfg.ServiceAuth.AudienceDIDs = append([]string(nil), bidderSpec.ServiceAuth.Audience...)
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
		if strings.TrimSpace(cfg.Network.CABundle) != "" {
			tlsConfig, err := bidderNetworkTLS(cfg.Network.CABundle)
			if err != nil {
				return Config{}, err
			}
			cfg.Deps.TLS = tlsConfig
			if deps.Log != nil {
				deps.Log.Info("the bidder trusts the network CA")
			}
		}
		if cfg.Network.IngressProxyHost != "" {
			cfg.Relay = &RelayConfig{
				IngressProxyHost: cfg.Network.IngressProxyHost,
				ConnectHost:      cfg.Network.IngressProxyHost,
				DialAddress:      ingressConnectHost(cfg.Network),
				PublicURLBase:    cfg.Network.GuestTLSURL(),
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
	cfg.Provider.GuestVaultServer = v1alpha1.VaultServer(bidderSpec.VaultRef, cfg.Provider.GuestVaultServer)
	if bidderSpec.Storage != nil && bidderSpec.Storage.DataDir != "" {
		cfg.StateDir = bidderSpec.Storage.DataDir
	}
	if bidderSpec.MaxConcurrentGuests > 0 {
		cfg.Provider.MaxConcurrentGuests = bidderSpec.MaxConcurrentGuests
	}
	cfg.Provider.GuestTLSURL = cfg.Network.GuestTLSURL()
	if port := portOf(cfg.Network.GuestTLSURL()); port > 0 {
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
	if cfg.PDSDPoP == nil && cfg.PDSAccessTokenRef != nil && deps.Credentials != nil {
		if credential, err := deps.Credentials.Resolve(context.Background(), *cfg.PDSAccessTokenRef); err == nil {
			if data, ok := credentialSessionData(credential.Data); ok {
				clientID := data.ClientID
				if clientID == "" {
					clientID = defaultBidderOAuthClientID
				}
				opts := oauthclient.Options{
					DID:        data.AccountDID,
					RepoDID:    data.AccountDID,
					PDSURL:     data.HostURL,
					ClientID:   clientID,
					Scopes:     data.Scopes,
					HTTPClient: externalHTTPClient(deps, DefaultCallTimeout),
				}
				if writer, ok := deps.Credentials.(component.CredentialWriter); ok {
					opts.Store = &bidderSessionStore{resolver: deps.Credentials, writer: writer, ref: *cfg.PDSAccessTokenRef}
				}
				session, err := oauthclient.New(data, opts)
				if err == nil {
					cfg.PDSTokens = session
					cfg.PDSDPoP = session
					if cfg.PDSURL == "" {
						cfg.PDSURL = data.HostURL
					}
				} else {
					if deps.Log != nil {
						deps.Log.Warn("the bidder could not restore the oauth session", "err", err.Error())
					}
					if data.AuthServerURL == "" || data.AuthServerTokenEndpoint == "" {
						authServer, tokenEndpoint, revokeEndpoint, discoverErr := oauthclient.DiscoverAuthServer(context.Background(), opts.HTTPClient, data.HostURL)
						if discoverErr == nil {
							data.AuthServerURL = authServer
							data.AuthServerTokenEndpoint = tokenEndpoint
							if revokeEndpoint != "" {
								data.AuthServerRevocationEndpoint = revokeEndpoint
							}
							if opts.Store != nil {
								_ = opts.Store.Save(context.Background(), data)
							}
						} else if deps.Log != nil {
							deps.Log.Warn("the bidder could not discover the oauth auth server", "err", discoverErr.Error())
						}
					}
					if pds, fallbackErr := indigoSessionPDS(data); fallbackErr == nil {
						cfg.PDS = pds
					}
				}
			} else {
				source, err := pdsclient.DPoPSourceFromData(credential.Data)
				if err != nil {
					if deps.Log != nil {
						deps.Log.Warn("the bidder could not read the session DPoP key", "err", err.Error())
					}
				} else {
					cfg.PDSDPoP = source
				}
			}
		}
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
	if name := hostnameOnly(n.GuestTLSURL()); name != "" {
		if address := bidderHostWithPort(n.GuestTLSURL(), n.Hosts); address != "" {
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

func bidderNetworkTLS(bundle string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(strings.TrimSpace(bundle))) {
		return nil, errors.New("bidder: the network CA bundle has no certificates")
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

func accessTokenRef(secret *commonv1.SecretRef, vaultRef *commonv1.ObjectRef) *component.CredentialRef {
	if secret == nil || strings.Trim(strings.TrimSpace(secret.Path), "/") == "" {
		return nil
	}
	ref := &component.CredentialRef{Mount: strings.Trim(strings.TrimSpace(secret.Mount), "/"), Path: strings.Trim(strings.TrimSpace(secret.Path), "/"),
		Keys: []string{"session", "accessJwt", "accessToken", "access_token", "token", "value",
			"dpop_privatekey_multibase", "dpopPrivateKeyMultibase"}}
	if key := strings.TrimSpace(secret.Key); key != "" {
		ref.Keys = []string{key}
	}
	ref.Server = v1alpha1.VaultServer(secret.Vault, "")
	if ref.Server == "" && vaultRef != nil {
		ref.Server = strings.TrimSpace(vaultRef.Name)
	}
	return ref
}

func (c Config) checkServiceAuthHost() error {
	if c.ServiceAuth.Hostname == "" || c.MarketURL == "" {
		return nil
	}
	override := c.ServiceAuth.Hostname
	if !strings.Contains(override, "://") {
		override = "//" + override
	}
	got := strings.ToLower(hostPortOf(override))
	want := strings.ToLower(hostPortOf(c.MarketURL))
	if got == "" || want == "" || got != want {
		return fmt.Errorf("bidder: serviceAuth.hostname %q must match the market URL host %q; the offering and the verifier audience have one source", c.ServiceAuth.Hostname, want)
	}
	return nil
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

func validateMarketComms(prefix string, comms *v1alpha1.MarketComms) error {
	if comms == nil || strings.TrimSpace(string(comms.Mode)) == "" {
		return nil
	}
	switch comms.Mode {
	case v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeFirehose, v1alpha1.MarketCommsModeBoth:
		return nil
	default:
		return fmt.Errorf("%s: unknown comms mode %q", prefix, comms.Mode)
	}
}

func (c Config) inboundXRPC() bool {
	if c.oauthIdentity() {
		return false
	}
	if c.Comms == nil {
		return true
	}
	switch c.Comms.Mode {
	case "", v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeBoth:
		return true
	}
	return false
}

func (c Config) outboundXRPC() bool {
	return true
}

func (c Config) oauthIdentity() bool {
	switch strings.TrimSpace(c.IdentityMode) {
	case string(v1alpha1.MarketIdentityModeOAuthQr), string(v1alpha1.MarketIdentityModeOAuthBrowser), string(v1alpha1.MarketIdentityModeOAuthDelegated):
		return true
	}
	return false
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

func hostPortOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func (c Config) bidTTL() time.Duration {
	if c.BidTTL > 0 {
		return c.BidTTL
	}
	return DefaultBidTTL
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
		c.GuestControlRef = &commonv1.ObjectRef{Kind: "GuestControlPlane", Name: DefaultGuestControlName}
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

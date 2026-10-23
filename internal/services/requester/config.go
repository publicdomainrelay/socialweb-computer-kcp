package requester

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
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const Kind = "Requester"

type IdentityMode string

const (
	IdentityModeLocal IdentityMode = "local"

	IdentityModeCallback IdentityMode = "callback"

	IdentityModeOAuth IdentityMode = "oauth"
)

const (
	DefaultServeHost = "127.0.0.1"

	DefaultMaxConcurrent = 64

	DefaultPerAccount = 16

	DefaultContractPoll = 2 * time.Second

	DefaultPolicyPoll = 250 * time.Millisecond

	DefaultPolicyTimeout = 2 * time.Minute

	DefaultVMReadyTimeout = 120 * time.Second

	DefaultHostKeysTimeout = 120 * time.Second

	DefaultBidWindow = 30 * time.Second

	DefaultReceiptTimeout = 5 * time.Minute

	DefaultReceiptPoll = 500 * time.Millisecond

	DefaultReleaseTimeout = 30 * time.Second

	DefaultReleaseRetryFirst = 5 * time.Second

	DefaultReleaseRetryMax = 5 * time.Minute

	DefaultReleaseBound = 24 * time.Hour

	DefaultHTTPTimeout = 30 * time.Second

	DefaultPolicyWorkflow = "requester-tangled-vouch"

	DefaultVMNamePrefix = "compute-"

	DefaultGuestUser = "root"

	DefaultGuestSSHCommand = "bash"

	DefaultShutdownTimeout = 30 * time.Second

	DefaultSessionCap = time.Hour

	DefaultSessionIdle = 5 * time.Minute

	DefaultDirectoryTTL = 60 * time.Second

	DefaultDirectoryKeys = 10_000

	PolicyRunTTLSeconds = int64(3600)

	ServiceAuthTTL = 5 * time.Minute

	MaxRecordBids = 20

	CapacityRetryDelay = 60 * time.Second

	MaxVMHostKeys = 8

	PolicyGhaLiteNSID = "computer.socialweb.temp.policy.ghalite"

	DefaultComputeDisk = "50G"

	DefaultComputeCPUs = 2

	DefaultComputeMem = "4G"

	FinalizerReleased = "requester.socialweb.computer/released"

	DefaultOAuthSessionFile = "oauth-session.json"
)

type Config struct {
	Deps component.Deps

	StateDir string

	Workspace string

	Namespace string

	ComponentName string

	Network v1alpha1.MarketNetworkValue

	Identity v1alpha1.ComponentIdentity

	IdentityMode IdentityMode

	LocalSigner bool

	DID string

	Handle string

	RepoDID string

	MarketDID string

	PDSRef *v1alpha1.ComponentRef

	RemotePDS *pdsv1alpha1.RemotePDS

	PolicyEngineRef *v1alpha1.ComponentRef

	VaultRef *v1alpha1.ComponentRef

	PDSURL string

	PDSAccessToken string

	PDSAccessTokenRef *component.CredentialRef

	PublicOrigin string

	MarketURL string

	OAuthClientID string

	OAuthRedirectURI string

	OAuthSessionPath string

	OAuthSessionData *oauthclient.SessionData

	OAuthScopes []string

	Directory identity.Directory

	DirectoryTTL time.Duration

	DirectoryKeys int

	HTTPClient *http.Client

	ServeHost string

	ServePort int

	PublicHostname string

	CallbackHostname string

	Repo RecordStore

	Contracts ContractStore

	PolicyRuns PolicyRunStore

	Market MarketClient

	RepoKey atcrypto.PrivateKey

	RepoKeyPath string

	AttestationKey atcrypto.PrivateKey

	AttestationKeyPath string

	PolicyWorkflow string

	PolicyArgs map[string]string

	PolicyTimeout time.Duration

	PolicyPoll time.Duration

	MaxConcurrent int

	PerAccount int

	ContractPoll time.Duration

	VMReadyTimeout time.Duration

	HostKeysTimeout time.Duration

	BidWindow time.Duration

	ReceiptTimeout time.Duration

	ReceiptPoll time.Duration

	FirstFree *bool

	BidderDIDs []string

	DenyBidderDIDs []string

	AcceptCopies int

	GuestAuthorizedKeys []string

	GuestUserData string

	GuestCAPEM string

	GuestHostAliases []string

	GuestKeyDir string

	OwnerSessionsFile string

	OwnerSessions map[string]sshtransport.OwnerSession

	IrohSocket string

	DumbpipePath string

	SSHDefaultCommand string

	ReleaseTimeout time.Duration

	ReleaseRetryFirst time.Duration

	ReleaseRetryMax time.Duration

	ReleaseBound time.Duration

	SessionCap time.Duration

	SessionIdle time.Duration

	ShutdownTimeout time.Duration

	Comms *v1alpha1.MarketComms

	Now func() time.Time

	Log *slog.Logger
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.RequesterSpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("requester: decoding the component spec: %w", err)
	}
	if err := validateMarketComms("requester", decoded.Comms); err != nil {
		return Config{}, err
	}
	cfg := Config{
		Deps:              deps,
		StateDir:          spec.StateDir,
		Workspace:         spec.Workspace,
		Namespace:         spec.Namespace,
		ComponentName:     spec.Name,
		PDSRef:            decoded.PdsRef,
		PDSAccessTokenRef: accessTokenRef(decoded.PDSAccessTokenPath, decoded.VaultRef),
		PolicyEngineRef:   decoded.PolicyEngineRef,
		VaultRef:          decoded.VaultRef,
		Comms:             decoded.Comms,
		BidderDIDs:        append([]string(nil), decoded.BidderDIDs...),
		PolicyArgs:        map[string]string{},
	}
	if cfg.StateDir == "" {
		cfg.StateDir = deps.StateDir
	}
	if decoded.Identity != nil {
		cfg.Identity = *decoded.Identity
		cfg.DID = decoded.Identity.Did
		cfg.Handle = decoded.Identity.Handle
	}
	if decoded.Network != nil {
		cfg.Network = *decoded.Network
		if hosts := networkResolver(cfg.Network, cfg.Deps.Hosts); hosts != nil {
			cfg.Deps.Hosts = hosts
		}
		if tlsConfig := networkTLSConfig(cfg.Network); tlsConfig != nil {
			cfg.Deps.TLS = tlsConfig
		}
		if path := strings.TrimSpace(cfg.Network.CaBundleRef); path != "" {
			if pem, err := os.ReadFile(path); err == nil {
				cfg.GuestCAPEM = strings.TrimSpace(string(pem))
			}
		}
	}
	if decoded.Serve != nil {
		if decoded.Serve.Host != "" {
			cfg.ServeHost = decoded.Serve.Host
		}
		cfg.ServePort = int(decoded.Serve.Port)
	}
	if decoded.Storage != nil && decoded.Storage.DataDir != "" {
		cfg.StateDir = decoded.Storage.DataDir
	}
	if cfg.DID == "" {
		cfg.DID = cfg.Identity.Did
	}
	cfg.RepoDID = cfg.DID
	cfg.MarketDID = cfg.DID
	cfg.IdentityMode = IdentityModeLocal
	if decoded.PdsRef != nil && decoded.PdsRef.Name != "" {
		cfg.IdentityMode = IdentityModeCallback
		if decoded.PdsRef.Did != "" {
			cfg.RepoDID = decoded.PdsRef.Did
			cfg.MarketDID = decoded.PdsRef.Did
		}
	}
	cfg.PublicOrigin = strings.TrimRight(strings.TrimSpace(decoded.PublicOrigin), "/")
	cfg.MarketURL = strings.TrimRight(strings.TrimSpace(decoded.MarketURL), "/")
	cfg.OAuthClientID = strings.TrimSpace(decoded.OAuthClientID)
	cfg.OAuthRedirectURI = strings.TrimSpace(decoded.OAuthRedirectURI)
	modeText := strings.ToLower(strings.TrimSpace(decoded.Mode))
	cfg.LocalSigner = modeText == "local" || modeText == "localrepo" ||
		(modeText == "" && (decoded.PdsRef == nil || decoded.PdsRef.Name == ""))
	switch modeText {
	case "", "local", "localrepo":
	case "callback":
		cfg.IdentityMode = IdentityModeCallback
	case "oauth", "oauthbrowser", "oauthqr":
		cfg.IdentityMode = IdentityModeOAuth
	default:
		return Config{}, fmt.Errorf("requester: unknown identity mode %q", decoded.Mode)
	}
	if cfg.IdentityMode == IdentityModeOAuth {
		cfg.RepoDID = cfg.DID
		cfg.MarketDID = cfg.DID
	}
	if cfg.IdentityMode == IdentityModeOAuth && deps.Credentials != nil && cfg.PDSAccessTokenRef != nil {
		credential, err := deps.Credentials.Resolve(context.Background(), *cfg.PDSAccessTokenRef)
		if err != nil {
			if deps.Log != nil {
				deps.Log.Warn("requester_could_not_read_the_oauth_session", "err", err)
			}
		} else if raw, ok := credential.Value("session"); ok && raw != "" {
			var data oauthclient.SessionData
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				if deps.Log != nil {
					deps.Log.Warn("requester_could_not_decode_the_oauth_session", "err", err)
				}
			} else if data.AccountDID != "" && data.HostURL != "" {
				cfg.OAuthSessionData = &data
				cfg.DID = data.AccountDID
				cfg.RepoDID = data.AccountDID
				cfg.MarketDID = data.AccountDID
			}
		}
	}
	if decoded.OwnerSessionsFile != "" {
		if cfg.IdentityMode == IdentityModeOAuth {
			return Config{}, errors.New("requester: ownerSessionsFile and the oauth identity mode exclude each other")
		}
		sessions, did, handle, err := sshtransport.LoadOwnerSessions(decoded.OwnerSessionsFile)
		if err != nil {
			return Config{}, err
		}
		session := sessions[did]
		if strings.TrimSpace(session.Service) == "" {
			return Config{}, fmt.Errorf("requester: the owner session of %s names no service", did)
		}
		cfg.OwnerSessionsFile = decoded.OwnerSessionsFile
		cfg.OwnerSessions = sessions
		cfg.DID = did
		cfg.Handle = handle
		cfg.RepoDID = did
		cfg.MarketDID = did
		if cfg.PolicyWorkflow == "" || cfg.PolicyWorkflow == DefaultPolicyWorkflow {
			cfg.PolicyWorkflow = "open"
		}
		cfg.PDSURL = session.Service
		cfg.IdentityMode = IdentityModeCallback
		cfg.LocalSigner = false
	}
	if decoded.IrohSocket != "" {
		cfg.IrohSocket = strings.TrimSpace(decoded.IrohSocket)
	}
	if decoded.AcceptCopies > 1 {
		cfg.AcceptCopies = int(decoded.AcceptCopies)
	}
	if decoded.MaxConcurrent > 0 {
		cfg.MaxConcurrent = int(decoded.MaxConcurrent)
	}
	if decoded.PerAccount > 0 {
		cfg.PerAccount = int(decoded.PerAccount)
	}
	if decoded.ContractPollMilliseconds != nil && *decoded.ContractPollMilliseconds > 0 {
		cfg.ContractPoll = time.Duration(*decoded.ContractPollMilliseconds) * time.Millisecond
	}
	if decoded.BidWindowSeconds != nil && *decoded.BidWindowSeconds > 0 {
		cfg.BidWindow = time.Duration(*decoded.BidWindowSeconds) * time.Second
	}
	if decoded.ReceiptTimeoutSeconds != nil && *decoded.ReceiptTimeoutSeconds > 0 {
		cfg.ReceiptTimeout = time.Duration(*decoded.ReceiptTimeoutSeconds) * time.Second
	}
	if decoded.ReceiptPollMilliseconds != nil && *decoded.ReceiptPollMilliseconds > 0 {
		cfg.ReceiptPoll = time.Duration(*decoded.ReceiptPollMilliseconds) * time.Millisecond
	}
	if decoded.SessionCapSeconds != nil && *decoded.SessionCapSeconds > 0 {
		cfg.SessionCap = time.Duration(*decoded.SessionCapSeconds) * time.Second
	}
	if decoded.SessionIdleSeconds != nil && *decoded.SessionIdleSeconds > 0 {
		cfg.SessionIdle = time.Duration(*decoded.SessionIdleSeconds) * time.Second
	}
	if decoded.DirectoryTTLSeconds != nil && *decoded.DirectoryTTLSeconds > 0 {
		cfg.DirectoryTTL = time.Duration(*decoded.DirectoryTTLSeconds) * time.Second
	}
	if decoded.DirectoryKeys != nil && *decoded.DirectoryKeys > 0 {
		cfg.DirectoryKeys = int(*decoded.DirectoryKeys)
	}
	if decoded.PolicyPollMilliseconds != nil && *decoded.PolicyPollMilliseconds > 0 {
		cfg.PolicyPoll = time.Duration(*decoded.PolicyPollMilliseconds) * time.Millisecond
	}
	if decoded.PolicyTimeoutSeconds != nil && *decoded.PolicyTimeoutSeconds > 0 {
		cfg.PolicyTimeout = time.Duration(*decoded.PolicyTimeoutSeconds) * time.Second
	}
	if len(decoded.OAuthScopes) > 0 {
		cfg.OAuthScopes = append([]string(nil), decoded.OAuthScopes...)
	}
	if decoded.GuestSSHKeysFile != "" {
		keys, err := loadGuestSSHKeys(decoded.GuestSSHKeysFile)
		if err != nil {
			return Config{}, err
		}
		cfg.GuestAuthorizedKeys = keys
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "root"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	cfg.defaults()
	return cfg, nil
}

type chainedResolver []networkdial.Resolver

func (c chainedResolver) Address(hostport string) (string, bool) {
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

func (c chainedResolver) Hosts() map[string]string {
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

func networkResolver(n v1alpha1.MarketNetworkValue, fallback networkdial.Resolver) component.NetworkHosts {
	entries := make([]networkdial.Host, 0, len(n.Hosts)+1)
	for _, host := range n.Hosts {
		entries = append(entries, networkdial.Host{Name: host.Name, Address: host.Address})
	}
	if name := hostnameOf(n.GuestTlsUrl); name != "" {
		if address := hostWithPort(n.GuestTlsUrl, n.Hosts); address != "" {
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
	return chainedResolver(resolvers)
}

func hostWithPort(raw string, hosts []v1alpha1.NetworkHost) string {
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

func networkTLSConfig(n v1alpha1.MarketNetworkValue) *tls.Config {
	path := strings.TrimSpace(n.CaBundleRef)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
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
	if out.Deps.StateDir == "" {
		out.Deps.StateDir = deps.StateDir
	}
	if out.StateDir == "" {
		out.StateDir = out.Deps.StateDir
	}
	if out.Log == nil {
		out.Log = out.Deps.Log
	}
	out.defaults()
	return out
}

func (c *Config) defaults() {
	if c.ServeHost == "" {
		c.ServeHost = DefaultServeHost
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.PerAccount <= 0 {
		c.PerAccount = DefaultPerAccount
	}
	if c.ContractPoll <= 0 {
		c.ContractPoll = DefaultContractPoll
	}
	if c.PolicyPoll <= 0 {
		c.PolicyPoll = DefaultPolicyPoll
	}
	if c.PolicyTimeout <= 0 {
		c.PolicyTimeout = DefaultPolicyTimeout
	}
	if c.VMReadyTimeout <= 0 {
		c.VMReadyTimeout = DefaultVMReadyTimeout
	}
	if c.HostKeysTimeout <= 0 {
		c.HostKeysTimeout = DefaultHostKeysTimeout
	}
	if c.BidWindow <= 0 {
		c.BidWindow = DefaultBidWindow
	}
	if c.ReceiptTimeout <= 0 {
		c.ReceiptTimeout = DefaultReceiptTimeout
	}
	if c.ReceiptPoll <= 0 {
		c.ReceiptPoll = DefaultReceiptPoll
	}
	if c.ReleaseTimeout <= 0 {
		c.ReleaseTimeout = DefaultReleaseTimeout
	}
	if c.ReleaseRetryFirst <= 0 {
		c.ReleaseRetryFirst = DefaultReleaseRetryFirst
	}
	if c.ReleaseRetryMax <= 0 {
		c.ReleaseRetryMax = DefaultReleaseRetryMax
	}
	if c.ReleaseBound <= 0 {
		c.ReleaseBound = DefaultReleaseBound
	}
	if c.SessionCap <= 0 {
		c.SessionCap = DefaultSessionCap
	}
	if c.SessionIdle <= 0 {
		c.SessionIdle = DefaultSessionIdle
	}
	if c.DirectoryTTL <= 0 {
		c.DirectoryTTL = DefaultDirectoryTTL
	}
	if c.DirectoryKeys <= 0 {
		c.DirectoryKeys = DefaultDirectoryKeys
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.PolicyWorkflow == "" {
		c.PolicyWorkflow = DefaultPolicyWorkflow
	}
	if c.SSHDefaultCommand == "" {
		c.SSHDefaultCommand = DefaultGuestSSHCommand
	}
	if c.GuestKeyDir == "" {
		if c.StateDir != "" {
			c.GuestKeyDir = filepath.Join(c.StateDir, "guest-keys")
		}
	}
	if c.RepoKeyPath == "" {
		if c.StateDir != "" {
			c.RepoKeyPath = filepath.Join(c.StateDir, "repo-key.json")
		}
	}
	if c.AttestationKeyPath == "" {
		if c.StateDir != "" {
			c.AttestationKeyPath = filepath.Join(c.StateDir, "attestation-key.json")
		}
	}
	if c.OAuthSessionPath == "" && c.StateDir != "" {
		c.OAuthSessionPath = filepath.Join(c.StateDir, DefaultOAuthSessionFile)
	}
	if len(c.OAuthScopes) == 0 {
		c.OAuthScopes = append([]string(nil), atprotooauth.RequesterOAuthScope...)
	}
	if c.IdentityMode == IdentityModeOAuth {
		if c.OAuthClientID == "" && c.PublicOrigin != "" {
			c.OAuthClientID = c.PublicOrigin + "/oauth-client-metadata.json"
		}
		if c.OAuthRedirectURI == "" && c.PublicOrigin != "" {
			c.OAuthRedirectURI = c.PublicOrigin + "/oauth/callback"
		}
	}
	if c.HTTPClient == nil {
		c.HTTPClient = externalHTTPClient(c.Deps, DefaultHTTPTimeout)
	}
	if c.PolicyArgs == nil {
		c.PolicyArgs = map[string]string{}
	}
	if c.FirstFree == nil {
		value := true
		c.FirstFree = &value
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil && c.Deps.Log != nil {
		c.Log = c.Deps.Log
	}
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
	if c.IdentityMode == IdentityModeOAuth {
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

func (c Config) firstFree() bool {
	return c.FirstFree == nil || *c.FirstFree
}

func (c Config) check() error {
	switch {
	case c.DID == "" && c.Handle == "" && c.IdentityMode != IdentityModeOAuth:
		return errors.New("requester: the component names no identity DID or handle")
	case c.ServeHost == "":
		return errors.New("requester: the component names no serve host")
	case c.IdentityMode != IdentityModeLocal && c.IdentityMode != IdentityModeCallback && c.IdentityMode != IdentityModeOAuth:
		return fmt.Errorf("requester: unknown identity mode %q", c.IdentityMode)
	}
	if c.IdentityMode != IdentityModeOAuth {
		return nil
	}
	switch {
	case c.OAuthClientID == "":
		return errors.New("requester: oauth identity mode names no OAuthClientID and no PublicOrigin")
	case c.OAuthRedirectURI == "":
		return errors.New("requester: oauth identity mode names no OAuthRedirectURI and no PublicOrigin")
	case !absoluteHTTPURL(c.OAuthClientID):
		return fmt.Errorf("requester: oauth client id %q is not an absolute http or https URL", c.OAuthClientID)
	case !absoluteHTTPURL(c.OAuthRedirectURI):
		return fmt.Errorf("requester: oauth redirect uri %q is not an absolute http or https URL", c.OAuthRedirectURI)
	case c.PublicOrigin != "" && !absoluteHTTPURL(c.PublicOrigin):
		return fmt.Errorf("requester: oauth public origin %q is not an absolute http or https URL", c.PublicOrigin)
	case c.OAuthSessionPath == "" && c.OAuthSessionData == nil:
		return errors.New("requester: oauth identity mode needs a state dir or an OAuthSessionPath")
	}
	return nil
}

func loadGuestSSHKeys(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("requester: the guest SSH keys file %s: %w", path, err)
	}
	keys := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys, nil
}

func absoluteHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
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
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func Factory(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
	cfg, err := ConfigFrom(deps, spec)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

func Register(runtime *component.Runtime) error {
	return runtime.Register(Kind, Factory)
}

func directoryFor(raw string, keys int, ttl time.Duration) identity.Directory {
	base := identity.DefaultDirectory()
	raw = strings.TrimRight(raw, "/")
	if raw == "" || raw == strings.TrimRight(identity.DefaultPLCURL, "/") {
		return base
	}
	cached, ok := base.(*identity.CacheDirectory)
	if !ok {
		return base
	}
	inner := cached.Inner
	if directory, ok := inner.(*identity.BaseDirectory); ok {
		inner = &identity.BaseDirectory{
			PLCURL:                 raw,
			PLCLimiter:             directory.PLCLimiter,
			DIDWebLimitFunc:        directory.DIDWebLimitFunc,
			HTTPClient:             directory.HTTPClient,
			PLCClient:              directory.PLCClient,
			TryAuthoritativeDNS:    directory.TryAuthoritativeDNS,
			SkipDNSDomainSuffixes:  append([]string(nil), directory.SkipDNSDomainSuffixes...),
			FallbackDNSServers:     append([]string(nil), directory.FallbackDNSServers...),
			SkipHandleVerification: directory.SkipHandleVerification,
			UserAgent:              directory.UserAgent,
		}
	}
	return identity.NewCacheDirectory(inner, keys, ttl, ttl, ttl)
}

func trustedHosts(network v1alpha1.MarketNetworkValue) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		if host := urlHost(raw); host != "" {
			if !seen[host] {
				seen[host] = true
				out = append(out, host)
			}
			return
		}
		raw = strings.TrimPrefix(raw, "[")
		raw = strings.TrimSuffix(raw, "]")
		if !seen[raw] {
			seen[raw] = true
			out = append(out, raw)
		}
	}
	for _, host := range network.Hosts {
		add(host.Name)
	}
	add(network.IngressProxyHost)
	add(network.GuestTlsUrl)
	add(network.QrOrigin)
	add(network.PlcDirectoryUrl)
	for _, raw := range network.RelayUrls {
		add(raw)
	}
	return out
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

func hostnameOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

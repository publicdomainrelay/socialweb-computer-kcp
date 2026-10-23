package requester

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "Requester"

type IdentityMode string

const (
	IdentityModeLocal IdentityMode = "local"

	IdentityModeCallback IdentityMode = "callback"

	IdentityModeOAuth IdentityMode = "oauth"
)

const (
	DefaultVMReadyTimeout = 120 * time.Second

	DefaultHostKeysTimeout = 120 * time.Second

	DefaultReceiptTimeout = 5 * time.Minute

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

func (s *Service) derive() error {
	spec := s.spec
	egress := s.deps.Egress
	egress.Log = cmp.Or(egress.Log, s.deps.Log)
	if spec.Network != nil {
		if hosts := networkResolver(*spec.Network, egress.Hosts); hosts != nil {
			egress.Hosts = hosts
		}
		if tlsConfig := networkTLSConfig(*spec.Network); tlsConfig != nil {
			egress.TLS = tlsConfig
		}
	}
	s.httpClient = egress.Client(DefaultHTTPTimeout)
	s.pdsAccessTokenRef = accessTokenRef(spec.PDSAccessToken, spec.VaultRef)
	if spec.Identity != nil {
		s.did = spec.Identity.Did
		s.handle = spec.Identity.Handle
	}
	s.repoDID = s.did
	s.marketDID = s.did
	s.identityMode = IdentityModeLocal
	callback := spec.PdsRef != nil && spec.PdsRef.Name != ""
	if callback {
		s.identityMode = IdentityModeCallback
		if did := strings.TrimSpace(spec.PdsDID); did != "" {
			s.repoDID = did
			s.marketDID = did
		}
	}
	publicOrigin := strings.TrimRight(strings.TrimSpace(spec.PublicOrigin), "/")
	s.oauthClientID = strings.TrimSpace(spec.OAuthClientID)
	s.oauthRedirectURI = strings.TrimSpace(spec.OAuthRedirectURI)
	modeText := strings.ToLower(strings.TrimSpace(spec.Mode))
	s.localSigner = modeText == "local" || modeText == "localrepo" || (modeText == "" && !callback)
	switch modeText {
	case "callback":
		s.identityMode = IdentityModeCallback
	case "oauth", "oauthbrowser", "oauthqr":
		s.identityMode = IdentityModeOAuth
	}
	if s.identityMode == IdentityModeOAuth {
		s.repoDID = s.did
		s.marketDID = s.did
	}
	if s.identityMode == IdentityModeOAuth && s.deps.Credentials != nil && s.pdsAccessTokenRef != nil {
		credential, err := s.deps.Credentials.Resolve(context.Background(), *s.pdsAccessTokenRef)
		if err != nil {
			s.log.Warn("requester_could_not_read_the_oauth_session", "err", err)
		} else if data, ok := oauthclient.SessionFromSecret(credential.Data); ok {
			s.oauthSessionData = &data
			s.did = data.AccountDID
			s.repoDID = data.AccountDID
			s.marketDID = data.AccountDID
		}
	}
	s.policyWorkflow = DefaultPolicyWorkflow
	if spec.OwnerSessionsFile != "" {
		if s.identityMode == IdentityModeOAuth {
			return errors.New("requester: ownerSessionsFile and the oauth identity mode exclude each other")
		}
		sessions, did, handle, err := sshtransport.LoadOwnerSessions(spec.OwnerSessionsFile)
		if err != nil {
			return err
		}
		session := sessions[did]
		if strings.TrimSpace(session.Service) == "" {
			return fmt.Errorf("requester: the owner session of %s names no service", did)
		}
		s.ownerSessions = sessions
		s.did = did
		s.handle = handle
		s.repoDID = did
		s.marketDID = did
		s.policyWorkflow = "open"
		s.pdsURL = session.Service
		s.identityMode = IdentityModeCallback
		s.localSigner = false
	}
	s.contractPoll = time.Duration(ptr.Deref(spec.ContractPollMilliseconds, 0)) * time.Millisecond
	s.bidWindow = time.Duration(ptr.Deref(spec.BidWindowSeconds, 0)) * time.Second
	s.receiptTimeout = time.Duration(ptr.Deref(spec.ReceiptTimeoutSeconds, 0)) * time.Second
	s.receiptPoll = time.Duration(ptr.Deref(spec.ReceiptPollMilliseconds, 0)) * time.Millisecond
	s.sessionCap = time.Duration(ptr.Deref(spec.SessionCapSeconds, 0)) * time.Second
	s.sessionIdle = time.Duration(ptr.Deref(spec.SessionIdleSeconds, 0)) * time.Second
	s.directoryTTL = time.Duration(ptr.Deref(spec.DirectoryTTLSeconds, 0)) * time.Second
	s.policyTimeout = time.Duration(ptr.Deref(spec.PolicyTimeoutSeconds, 0)) * time.Second
	s.oauthScopes = append([]string(nil), spec.OAuthScopes...)
	if len(s.oauthScopes) == 0 {
		s.oauthScopes = append([]string(nil), atprotooauth.RequesterOAuthScope...)
	}
	if spec.GuestSSHKeysFile != "" {
		keys, err := loadGuestSSHKeys(spec.GuestSSHKeysFile)
		if err != nil {
			return err
		}
		s.guestAuthorizedKeys = keys
	}
	if s.identityMode == IdentityModeOAuth && publicOrigin != "" {
		if s.oauthClientID == "" {
			s.oauthClientID = publicOrigin + "/oauth-client-metadata.json"
		}
		if s.oauthRedirectURI == "" {
			s.oauthRedirectURI = publicOrigin + "/oauth/callback"
		}
	}
	return s.check(publicOrigin)
}

func (s *Service) check(publicOrigin string) error {
	if s.did == "" && s.handle == "" && s.identityMode != IdentityModeOAuth {
		return errors.New("requester: the component names no identity DID or handle")
	}
	if s.identityMode != IdentityModeOAuth {
		return nil
	}
	switch {
	case s.oauthClientID == "":
		return errors.New("requester: oauth identity mode names no OAuthClientID and no PublicOrigin")
	case s.oauthRedirectURI == "":
		return errors.New("requester: oauth identity mode names no OAuthRedirectURI and no PublicOrigin")
	case !absoluteHTTPURL(s.oauthClientID):
		return fmt.Errorf("requester: oauth client id %q is not an absolute http or https URL", s.oauthClientID)
	case !absoluteHTTPURL(s.oauthRedirectURI):
		return fmt.Errorf("requester: oauth redirect uri %q is not an absolute http or https URL", s.oauthRedirectURI)
	case publicOrigin != "" && !absoluteHTTPURL(publicOrigin):
		return fmt.Errorf("requester: oauth public origin %q is not an absolute http or https URL", publicOrigin)
	case s.oauthSessionPath() == "" && s.oauthSessionData == nil:
		return errors.New("requester: oauth identity mode needs a state dir or an OAuthSessionPath")
	}
	return nil
}

func (s *Service) workspace() string {
	return cmp.Or(s.deps.Self.Workspace, "root")
}

func (s *Service) network() v1alpha1.MarketNetworkValue {
	return ptr.Deref(s.spec.Network, v1alpha1.MarketNetworkValue{})
}

func (s *Service) stateDir() string {
	if s.spec.Storage != nil && s.spec.Storage.DataDir != "" {
		return s.spec.Storage.DataDir
	}
	return s.deps.StateDir
}

func (s *Service) stateFile(name string) string {
	if dir := s.stateDir(); dir != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

func (s *Service) oauthSessionPath() string {
	return s.stateFile(DefaultOAuthSessionFile)
}

func (s *Service) guestKeyDir() string {
	return s.stateFile("guest-keys")
}

func (s *Service) guestCAPEM() string {
	return strings.TrimSpace(s.network().CABundle)
}

func (s *Service) inboundXRPC() bool {
	if s.identityMode == IdentityModeOAuth {
		return false
	}
	if s.spec.Comms == nil {
		return true
	}
	switch s.spec.Comms.Mode {
	case "", v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeBoth:
		return true
	}
	return false
}

func (s *Service) outboundXRPC() bool {
	return true
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
	if name := hostnameOf(n.GuestTLSURL()); name != "" {
		if address := hostport.ForURL(n.GuestTLSURL(), n.Hosts); address != "" {
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

func networkTLSConfig(n v1alpha1.MarketNetworkValue) *tls.Config {
	bundle := strings.TrimSpace(n.CABundle)
	if bundle == "" {
		return nil
	}
	cfg, err := httpclient.TLSConfig(httpclient.TLSOptions{CAPEM: []byte(bundle)})
	if err != nil {
		return nil
	}
	return cfg
}

func accessTokenRef(secret *commonv1.SecretRef, vaultRef *commonv1.ObjectRef) *component.CredentialRef {
	if secret == nil || strings.Trim(strings.TrimSpace(secret.Path), "/") == "" {
		return nil
	}
	ref := &component.CredentialRef{Mount: strings.Trim(strings.TrimSpace(secret.Mount), "/"), Path: strings.Trim(strings.TrimSpace(secret.Path), "/"),
		Keys: []string{"session", "accessJwt", "accessToken", "access_token", "token", "value"}}
	if key := strings.TrimSpace(secret.Key); key != "" {
		ref.Keys = []string{key}
	}
	ref.Server = v1alpha1.VaultServer(secret.Vault, "")
	if ref.Server == "" && vaultRef != nil {
		ref.Server = strings.TrimSpace(vaultRef.Name)
	}
	return ref
}

func loadGuestSSHKeys(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("requester: the guest SSH keys file %s: %w", path, err)
	}
	keys := []string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
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
	add(network.GuestTLSURL())
	add(network.QrOrigin)
	add(network.PLCDirectoryURL)
	for _, raw := range network.Relays() {
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

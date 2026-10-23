package bidder

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "Bidder"

const (
	RecordReplyLimit = 8 << 20

	RecordRequestTimeout = 30 * time.Second

	DefaultGuestLoginMount = computevm.GuestLoginDefaultMount

	DefaultGuestVaultServer = computevm.VaultServer

	DefaultGuestVaultMount = computevm.VaultMount

	DefaultBootTimeout = 2 * time.Minute

	DefaultReleaseTimeout = 150 * time.Second

	guestControlKind = "GuestControlPlane"

	guestControlName = "guest-kcp"

	vaultClientName = "bidder"
)

type ProviderConfig struct {
	Handle string

	GuestTLSPort int

	GuestVaultServer string

	StopGraceSeconds int32

	MaxConcurrentGuests int32
}

type Deps struct {
	component.Instance

	KCP component.KCPClients

	Credentials component.CredentialResolver

	Egress component.Egress
}

func New(spec v1alpha1.BidderSpec, deps Deps) (*Service, error) {
	policyArgs, err := parsePolicyArgs(spec.PolicyArgs)
	if err != nil {
		return nil, err
	}
	handle := ""
	if spec.Identity != nil {
		handle = spec.Identity.Handle
	}
	providerHandle := handle
	if spec.Kcp != nil && spec.Kcp.Handle != "" {
		providerHandle = spec.Kcp.Handle
		handle = cmp.Or(handle, spec.Kcp.Handle)
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	base := deps.Egress
	if base.Log == nil {
		base.Log = deps.Log
	}
	egress := base
	if spec.Network != nil {
		if hosts := bidderNetworkResolver(*spec.Network, egress.Hosts); hosts != nil {
			egress.Hosts = hosts
		}
		if strings.TrimSpace(spec.Network.CABundle) != "" {
			tlsConfig, err := bidderNetworkTLS(spec.Network.CABundle)
			if err != nil {
				return nil, err
			}
			egress.TLS = tlsConfig
			log.Info("the bidder trusts the network CA")
		}
	}
	s := &Service{
		spec:             spec,
		deps:             deps,
		egress:           egress,
		workspace:        cmp.Or(deps.Self.Workspace, "root"),
		handle:           handle,
		providerHandle:   providerHandle,
		guestVaultServer: v1alpha1.VaultServer(spec.VaultRef, DefaultGuestVaultServer),
		guestTLSPort:     portOf(spec.Network.GuestTLSURL()),
		tokenRef:         accessTokenRef(spec.PDSAccessToken, spec.VaultRef),
		policyArgs:       policyArgs,
		policyTimeout:    time.Duration(ptr.Deref(spec.PolicyTimeoutSeconds, 0)) * time.Second,
		heartbeat:        time.Duration(ptr.Deref(spec.HeartbeatIntervalSeconds, 0)) * time.Second,
		offeringRefresh:  time.Duration(ptr.Deref(spec.OfferingRefreshSeconds, 0)) * time.Second,
		bidTTL:           time.Duration(ptr.Deref(spec.BidTTLSeconds, 0)) * time.Second,
	}
	if spec.ServiceAuth != nil {
		s.serviceAuthHost = strings.TrimSpace(spec.ServiceAuth.Hostname)
	}
	s.restoreSession(base, log)
	switch {
	case s.handle == "":
		return nil, errors.New("bidder: the component names no handle")
	case s.guestVaultServer == "":
		return nil, errors.New("bidder: the provider names no guest vault server")
	}
	s.log = log.With("component", "bidder", "handle", s.handle)
	return s, nil
}

func (s *Service) restoreSession(egress component.Egress, log *slog.Logger) {
	credentials := s.deps.Credentials
	if s.tokenRef == nil || credentials == nil {
		return
	}
	credential, err := credentials.Resolve(context.Background(), *s.tokenRef)
	if err != nil {
		return
	}
	data, ok := oauthclient.SessionFromSecret(credential.Data)
	if !ok {
		source, err := pdsclient.DPoPSourceFromData(credential.Data)
		if err != nil {
			log.Warn("the bidder could not read the session DPoP key", "err", err.Error())
			return
		}
		s.pdsDPoP = source
		return
	}
	opts := oauthclient.Options{
		DID:        data.AccountDID,
		RepoDID:    data.AccountDID,
		PDSURL:     data.HostURL,
		ClientID:   cmp.Or(data.ClientID, defaultBidderOAuthClientID),
		Scopes:     data.Scopes,
		HTTPClient: egress.Client(DefaultCallTimeout),
	}
	if writer, ok := credentials.(component.CredentialWriter); ok {
		opts.Store = &bidderSessionStore{resolver: credentials, writer: writer, ref: *s.tokenRef}
	}
	session, err := oauthclient.New(data, opts)
	if err == nil {
		s.pdsTokens = session
		s.pdsDPoP = session
		s.pdsURL = data.HostURL
		return
	}
	log.Warn("the bidder could not restore the oauth session", "err", err.Error())
	if data.AuthServerURL != "" && data.AuthServerTokenEndpoint != "" {
		return
	}
	authServer, tokenEndpoint, revokeEndpoint, discoverErr := oauthclient.DiscoverAuthServer(context.Background(), opts.HTTPClient, data.HostURL)
	if discoverErr != nil {
		log.Warn("the bidder could not discover the oauth auth server", "err", discoverErr.Error())
		return
	}
	data.AuthServerURL = authServer
	data.AuthServerTokenEndpoint = tokenEndpoint
	if revokeEndpoint != "" {
		data.AuthServerRevocationEndpoint = revokeEndpoint
	}
	if opts.Store != nil {
		_ = opts.Store.Save(context.Background(), data)
	}
}

func (s *Service) stateDir() string {
	if s.spec.Storage != nil && s.spec.Storage.DataDir != "" {
		return s.spec.Storage.DataDir
	}
	return s.deps.StateDir
}

func (s *Service) network() v1alpha1.MarketNetworkValue {
	return ptr.Deref(s.spec.Network, v1alpha1.MarketNetworkValue{})
}

func (s *Service) identityRef() *commonv1.ObjectRef {
	if s.spec.Identity != nil {
		return s.spec.Identity.MarketIdentityRef
	}
	return s.spec.PdsRef
}

func (s *Service) stopGraceSeconds() int32 {
	if s.spec.Kcp == nil {
		return 0
	}
	return s.spec.Kcp.StopGraceSeconds
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
	if name := hostport.URLHostname(n.GuestTLSURL()); name != "" {
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
	return bidderChainedResolver(resolvers)
}

func bidderNetworkTLS(bundle string) (*tls.Config, error) {
	cfg, err := httpclient.TLSConfig(httpclient.TLSOptions{CAPEM: []byte(strings.TrimSpace(bundle))})
	if err != nil || cfg.RootCAs == nil {
		return nil, errors.New("bidder: the network CA bundle has no certificates")
	}
	return cfg, nil
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

func (s *Service) checkServiceAuthHost() error {
	if s.serviceAuthHost == "" || s.marketURL == "" {
		return nil
	}
	override := s.serviceAuthHost
	if !strings.Contains(override, "://") {
		override = "//" + override
	}
	got := strings.ToLower(hostPortOf(override))
	want := strings.ToLower(hostPortOf(s.marketURL))
	if got == "" || want == "" || got != want {
		return fmt.Errorf("bidder: serviceAuth.hostname %q must match the market URL host %q; the offering and the verifier audience have one source", s.serviceAuthHost, want)
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

func (s *Service) inboundXRPC() bool {
	if s.oauthIdentity() {
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

func (s *Service) oauthIdentity() bool {
	switch strings.TrimSpace(s.spec.Mode) {
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

func isAbsoluteURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

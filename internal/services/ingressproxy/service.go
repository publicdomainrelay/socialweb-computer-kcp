package ingressproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/iroh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/opentunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/xrpcsubscription"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsreload"
)

const Kind = "IngressProxy"

const DefaultHost = "127.0.0.1"

const DefaultShutdownTimeout = 5 * time.Second

type Config struct {
	Deps              component.Deps
	StateDir          string
	Workspace         string
	Address           string
	Host              string
	Port              int
	Hostname          string
	Hostnames         []string
	IngressProxyHost  string
	PublicHost        string
	Network           *v1alpha1.MarketNetworkValue
	RelayRef          *v1alpha1.ComponentRef
	RelayURL          string
	ServiceID         string
	AdditionalHosts   []string
	AllowedDIDs       []string
	AllowedHosts      []string
	PLCURL            string
	Directory         identity.Directory
	CertFile          string
	KeyFile           string
	TLS               *tls.Config
	RelayTimeout      time.Duration
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	ReconnectGrace    time.Duration
	NonceTTL          time.Duration
	ShutdownTimeout   time.Duration
	Provider          string
	Iroh              *v1alpha1.IngressProxyIroh
	OpenTunnel        *v1alpha1.IngressProxyOpenTunnel
	Identity          exposure.IngressIdentity
}

type Service struct {
	cfg Config

	mu sync.Mutex

	provider Provider

	providerName string
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg}
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.IngressProxySpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("ingressproxy: decoding the component spec: %w", err)
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	workspace := strings.TrimSpace(spec.Workspace)
	if workspace == "" {
		workspace = "root"
	}
	cfg := Config{
		Deps:             deps,
		StateDir:         stateDir,
		Workspace:        workspace,
		Host:             DefaultHost,
		Hostnames:        append([]string(nil), decoded.Hostnames...),
		IngressProxyHost: strings.TrimSpace(decoded.IngressProxyHost),
		PublicHost:       strings.TrimSpace(decoded.PublicHost),
		RelayRef:         decoded.RelayRef,
		RelayURL:         strings.TrimSpace(decoded.RelayURL),
		Provider:         decoded.Provider,
		Iroh:             copyIroh(decoded.Iroh),
		OpenTunnel:       copyOpenTunnel(decoded.OpenTunnel),
	}
	if decoded.RelayTimeoutMilliseconds != nil && *decoded.RelayTimeoutMilliseconds > 0 {
		cfg.RelayTimeout = time.Duration(*decoded.RelayTimeoutMilliseconds) * time.Millisecond
	}
	if decoded.KeepaliveMilliseconds != nil && *decoded.KeepaliveMilliseconds > 0 {
		cfg.KeepaliveInterval = time.Duration(*decoded.KeepaliveMilliseconds) * time.Millisecond
	}
	if decoded.KeepaliveTimeoutMilliseconds != nil && *decoded.KeepaliveTimeoutMilliseconds > 0 {
		cfg.KeepaliveTimeout = time.Duration(*decoded.KeepaliveTimeoutMilliseconds) * time.Millisecond
	}
	if decoded.ReconnectGraceMilliseconds != nil && *decoded.ReconnectGraceMilliseconds > 0 {
		cfg.ReconnectGrace = time.Duration(*decoded.ReconnectGraceMilliseconds) * time.Millisecond
	}
	if decoded.NonceTTLMilliseconds != nil && *decoded.NonceTTLMilliseconds > 0 {
		cfg.NonceTTL = time.Duration(*decoded.NonceTTLMilliseconds) * time.Millisecond
	}
	if decoded.Network != nil {
		network := *decoded.Network
		cfg.Network = &network
		cfg.PLCURL = strings.TrimRight(strings.TrimSpace(decoded.Network.PlcDirectoryUrl), "/")
		if cfg.IngressProxyHost == "" {
			cfg.IngressProxyHost = strings.TrimSpace(decoded.Network.IngressProxyHost)
		}
		if cfg.Hostname == "" {
			cfg.Hostname = hostnameOnly(strings.TrimSpace(decoded.Network.IngressProxyHost))
		}
		if cfg.RelayURL == "" && len(decoded.Network.RelayUrls) > 0 {
			cfg.RelayURL = strings.TrimSpace(decoded.Network.RelayUrls[0])
		}
	}
	if cfg.Hostname == "" {
		cfg.Hostname = hostnameOnly(cfg.IngressProxyHost)
	}
	if cfg.Hostname == "" && len(cfg.Hostnames) > 0 {
		cfg.Hostname = hostnameOnly(strings.TrimSpace(cfg.Hostnames[0]))
	}
	if cfg.Hostname == "" && decoded.Identity != nil {
		cfg.Hostname = hostnameOnly(strings.TrimSpace(decoded.Identity.Handle))
	}
	if cfg.Hostname == "" && decoded.IdentityRef != nil {
		cfg.Hostname = hostnameOnly(strings.TrimSpace(decoded.IdentityRef.Did))
	}
	if decoded.Identity != nil && strings.TrimSpace(decoded.Identity.Did) != "" {
		cfg.AllowedDIDs = appendValues(cfg.AllowedDIDs, decoded.Identity.Did)
	}
	if decoded.IdentityRef != nil && strings.TrimSpace(decoded.IdentityRef.Did) != "" {
		cfg.AllowedDIDs = appendValues(cfg.AllowedDIDs, decoded.IdentityRef.Did)
	}
	if decoded.RelayRef != nil && strings.TrimSpace(decoded.RelayRef.Did) != "" {
		cfg.AllowedDIDs = appendValues(cfg.AllowedDIDs, decoded.RelayRef.Did)
	}
	if host := hostnameOnly(cfg.IngressProxyHost); host != "" {
		cfg.AllowedHosts = appendValues(cfg.AllowedHosts, host, "*."+host)
	}
	if host := hostnameOnly(cfg.PublicHost); host != "" {
		cfg.AllowedHosts = appendValues(cfg.AllowedHosts, host)
		cfg.Hostnames = appendValues(cfg.Hostnames, host)
	}
	if decoded.Storage != nil && decoded.Storage.DataDir != "" {
		cfg.StateDir = decoded.Storage.DataDir
	}
	if decoded.Serve != nil {
		if decoded.Serve.Host != "" {
			cfg.Host = decoded.Serve.Host
		}
		cfg.Port = int(decoded.Serve.Port)
	}
	if decoded.TLS != nil {
		cfg.Hostnames = appendValues(cfg.Hostnames, decoded.TLS.Hostnames...)
	}
	if decoded.Network != nil {
		cfg.Hostnames = appendValues(cfg.Hostnames, networkHostnames(decoded.Network, cfg.Port)...)
	}
	cfg.AdditionalHosts = appendValues(cfg.AdditionalHosts, additionalHosts(primaryHostname(cfg.Hostname, cfg.Hostnames), cfg.Hostnames)...)
	if wildcard := wildcardHost(cfg.Hostnames); wildcard != "" {
		cfg.AllowedHosts = appendValues(cfg.AllowedHosts, wildcard)
	}
	if decoded.RelayRef != nil {
		if did := strings.TrimSpace(decoded.RelayRef.Did); did != "" {
			cfg.AllowedDIDs = appendValues(cfg.AllowedDIDs, did)
		}
		if relayURL, ok := relayEndpoint(cfg.Deps, cfg.Workspace, decoded.RelayRef); ok {
			cfg.RelayURL = relayURL
		}
	}
	if decoded.TLS != nil && decoded.TLS.Enabled {
		certFile, keyFile := certificateFiles(decoded.TLS.CertRef, decoded.Storage, cfg.Hostnames)
		cfg.CertFile = certFile
		cfg.KeyFile = keyFile
		tlsConfig, err := serverTLS(deps, cfg.CertFile)
		if err != nil {
			return Config{}, fmt.Errorf("ingressproxy: %w", err)
		}
		cfg.TLS = tlsConfig
	}
	return cfg, nil
}

func copyIroh(in *v1alpha1.IngressProxyIroh) *v1alpha1.IngressProxyIroh {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func copyOpenTunnel(in *v1alpha1.IngressProxyOpenTunnel) *v1alpha1.IngressProxyOpenTunnel {
	if in == nil {
		return nil
	}
	out := *in
	out.Command = append([]string(nil), in.Command...)
	return &out
}

func appendValues(values []string, extra ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(extra))
	out := make([]string, 0, len(values)+len(extra))
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	for _, value := range values {
		add(value)
	}
	for _, value := range extra {
		add(value)
	}
	return out
}

func additionalHosts(primary string, hostnames []string) []string {
	wanted := strings.ToLower(hostnameOnly(primary))
	var out []string
	for _, host := range hostnames {
		if strings.ToLower(hostnameOnly(host)) == wanted {
			continue
		}
		out = append(out, host)
	}
	return out
}

func wildcardHost(hostnames []string) string {
	for _, host := range hostnames {
		if strings.TrimSpace(host) == "*" {
			return "*"
		}
	}
	return ""
}

func networkHostnames(network *v1alpha1.MarketNetworkValue, port int) []string {
	if network == nil || port <= 0 {
		return nil
	}
	wanted := strconv.Itoa(port)
	var out []string
	for _, host := range network.Hosts {
		_, value, err := net.SplitHostPort(host.Address)
		if err != nil || value != wanted {
			continue
		}
		out = append(out, host.Name)
	}
	return out
}

func relayEndpoint(deps component.Deps, workspace string, ref *v1alpha1.ComponentRef) (string, bool) {
	if ref == nil {
		return "", false
	}
	kind := strings.TrimSpace(ref.Kind)
	if kind == "" {
		kind = "Relay"
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", false
	}
	if deps.Registry == nil {
		return "", false
	}
	if workspace != "" {
		for _, wanted := range []string{"http", "relay", "subscribeRepos"} {
			if endpoint, ok := deps.Registry.Endpoint(workspace, kind, name, wanted); ok {
				return endpointURL(endpoint)
			}
		}
	}
	for _, entry := range deps.Registry.List("") {
		if entry.Key.Kind != kind || entry.Key.Name != name {
			continue
		}
		for _, endpoint := range entry.Endpoints {
			if endpoint.URL != "" {
				return endpoint.URL, true
			}
		}
		for _, endpoint := range entry.Endpoints {
			if endpoint.Address != "" {
				return endpointURL(endpoint)
			}
		}
	}
	return "", false
}

func endpointURL(endpoint component.Endpoint) (string, bool) {
	if endpoint.URL != "" {
		return endpoint.URL, true
	}
	if endpoint.Address == "" {
		return "", false
	}
	scheme := strings.TrimSpace(endpoint.Protocol)
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + endpoint.Address, true
}

func certificateFiles(certRef string, storage *v1alpha1.ComponentStorage, hostnames []string) (string, string) {
	certFile := refPath(certRef, storage)
	if certFile == "" && storage != nil && storage.CertsDir != "" && len(hostnames) > 0 {
		name := hostnameOnly(hostnames[0])
		if name != "" && name != "*" {
			certFile = filepath.Join(storage.CertsDir, name+".crt")
		}
	}
	return certFile, keyFileFor(certFile)
}

func refPath(ref string, storage *v1alpha1.ComponentStorage) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if filepath.IsAbs(ref) || storage == nil || storage.CertsDir == "" {
		return ref
	}
	return filepath.Join(storage.CertsDir, ref)
}

func keyFileFor(certFile string) string {
	switch {
	case certFile == "":
		return ""
	case strings.HasSuffix(certFile, ".crt"):
		return strings.TrimSuffix(certFile, ".crt") + ".key"
	case strings.HasSuffix(certFile, ".pem"):
		return strings.TrimSuffix(certFile, ".pem") + ".key"
	default:
		return certFile + ".key"
	}
}

func keyFilesFor(certFile string) []string {
	candidates := []string{keyFileFor(certFile)}
	if strings.HasSuffix(certFile, ".pem") {
		candidates = append(candidates, strings.TrimSuffix(certFile, ".pem")+"-key.pem")
	}
	return append(candidates, certFile)
}

func serverTLS(deps component.Deps, certFile string) (*tls.Config, error) {
	if certFile != "" {
		reloader, err := tlsreload.New(certFile, keyFilesFor(certFile), deps.Log)
		if err != nil {
			return nil, err
		}
		return reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}), nil
	}
	if deps.TLS != nil {
		tlsConfig := deps.TLS.Clone()
		if len(tlsConfig.NextProtos) == 0 {
			tlsConfig.NextProtos = []string{"http/1.1"}
		}
		return tlsConfig, nil
	}
	return nil, errors.New("a TLS certificate is required")
}

func Factory(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
	cfg, err := ConfigFrom(deps, spec)
	if err != nil {
		return nil, err
	}
	return New(cfg), nil
}

func Register(runtime *component.Runtime, identity exposure.IngressIdentity) error {
	return runtime.Register(Kind, func(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
		cfg, err := ConfigFrom(deps, spec)
		if err != nil {
			return nil, err
		}
		cfg.Identity = identity
		return New(cfg), nil
	})
}

func buildProvider(cfg Config) (Provider, string, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if name == "" {
		switch {
		case cfg.OpenTunnel != nil:
			name = v1alpha1.IngressProviderOpenTunnel
		case strings.TrimSpace(cfg.IngressProxyHost) != "":
			name = v1alpha1.IngressProviderXRPCSubscription
		default:
			return nil, "", errors.New("ingressproxy: no provider selected: set spec.provider, spec.xrpc.ingressProxyHost or spec.opentunnel")
		}
	}
	switch name {
	case v1alpha1.IngressProviderXRPCSubscription:
		provider := xrpcsubscription.New(xrpcsubscription.Config{
			Address:           cfg.Address,
			Host:              cfg.Host,
			Port:              cfg.Port,
			Hostname:          cfg.Hostname,
			Hostnames:         append([]string(nil), cfg.Hostnames...),
			ServiceID:         cfg.ServiceID,
			AdditionalHosts:   append([]string(nil), cfg.AdditionalHosts...),
			AllowedDIDs:       append([]string(nil), cfg.AllowedDIDs...),
			AllowedHosts:      append([]string(nil), cfg.AllowedHosts...),
			PLCURL:            cfg.PLCURL,
			Directory:         cfg.Directory,
			CertFile:          cfg.CertFile,
			KeyFile:           cfg.KeyFile,
			TLS:               cfg.TLS,
			RelayURL:          cfg.RelayURL,
			RelayTimeout:      cfg.RelayTimeout,
			KeepaliveInterval: cfg.KeepaliveInterval,
			KeepaliveTimeout:  cfg.KeepaliveTimeout,
			ReconnectGrace:    cfg.ReconnectGrace,
			NonceTTL:          cfg.NonceTTL,
			ShutdownTimeout:   cfg.ShutdownTimeout,
			Identity:          cfg.Identity,
		})
		return provider, v1alpha1.IngressProviderXRPCSubscription, nil
	case v1alpha1.IngressProviderIroh:
		return nil, "", errors.New("ingressproxy: provider iroh is not supported yet; select xrpc-subscription or opentunnel")
	case v1alpha1.IngressProviderOpenTunnel:
		if cfg.OpenTunnel == nil {
			return nil, "", errors.New("ingressproxy: provider opentunnel requires spec.opentunnel")
		}
		providerCfg := opentunnel.Config{
			Command:       append([]string(nil), cfg.OpenTunnel.Command...),
			RouteName:     strings.TrimSpace(cfg.OpenTunnel.RouteName),
			ProxyProtocol: strings.TrimSpace(cfg.OpenTunnel.ProxyProtocol),
			LocalHost:     strings.TrimSpace(cfg.OpenTunnel.LocalHost),
		}
		if err := providerCfg.Check(); err != nil {
			return nil, "", err
		}
		return opentunnel.New(providerCfg), v1alpha1.IngressProviderOpenTunnel, nil
	default:
		return nil, "", fmt.Errorf("ingressproxy: unknown provider %q (use %s, %s or %s)", cfg.Provider,
			v1alpha1.IngressProviderXRPCSubscription, v1alpha1.IngressProviderIroh, v1alpha1.IngressProviderOpenTunnel)
	}
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := s.cfg
	cfg.Deps = mergeDeps(cfg.Deps, deps)
	if cfg.Workspace == "" {
		cfg.Workspace = "root"
	}
	if cfg.RelayRef != nil {
		if relayURL, ok := relayEndpoint(cfg.Deps, cfg.Workspace, cfg.RelayRef); ok {
			cfg.RelayURL = relayURL
		}
	}
	provider, name, err := buildProvider(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.provider = provider
	s.providerName = name
	s.mu.Unlock()
	return provider.Start(ctx, cfg.Deps)
}

func (s *Service) Ready(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	provider := s.provider
	s.mu.Unlock()
	if provider == nil {
		return errors.New("ingressproxy: the service is not started")
	}
	if ready, ok := provider.(interface{ Ready(context.Context) error }); ok {
		return ready.Ready(ctx)
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	provider := s.provider
	name := s.providerName
	publicURL := s.publicURLLocked()
	s.mu.Unlock()
	if provider == nil {
		return nil
	}
	out := provider.Endpoints()
	if name != "" {
		out = append(out, component.Endpoint{Name: "provider", Address: name})
	}
	if publicURL != "" {
		out = append(out, component.Endpoint{Name: "url", URL: publicURL})
	}
	return out
}

func (s *Service) publicURLLocked() string {
	if value := strings.TrimSpace(s.cfg.PublicHost); value != "" {
		if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
			return parsed.Scheme + "://" + parsed.Host
		}
		return "https://" + hostnameOnly(value)
	}
	host := hostnameOnly(primaryHostname(s.cfg.Hostname, s.cfg.Hostnames))
	if host == "" || host == "localhost" {
		return ""
	}
	return "https://" + host
}

func (s *Service) Provider() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerName
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	provider := s.provider
	s.mu.Unlock()
	if provider == nil {
		return nil
	}
	return provider.Stop(ctx)
}

func (s *Service) IngressRegistrar() exposure.IngressRegistrar {
	s.mu.Lock()
	provider := s.provider
	s.mu.Unlock()
	if provider == nil {
		return nil
	}
	return provider.IngressRegistrar()
}

func (s *Service) IngressProvider() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerName
}

func mergeDeps(cfgDeps, deps component.Deps) component.Deps {
	out := cfgDeps
	if out.Log == nil {
		out.Log = deps.Log
	}
	if out.TLS == nil {
		out.TLS = deps.TLS
	}
	if out.StateDir == "" {
		out.StateDir = deps.StateDir
	}
	if out.Registry == nil {
		out.Registry = deps.Registry
	}
	if out.Hosts == nil {
		out.Hosts = deps.Hosts
	}
	if out.KCP == nil {
		out.KCP = deps.KCP
	}
	if out.Credentials == nil {
		out.Credentials = deps.Credentials
	}
	if out.Vault == nil {
		out.Vault = deps.Vault
	}
	return out
}

func primaryHostname(host string, hostnames []string) string {
	if host != "" {
		return host
	}
	for _, candidate := range hostnames {
		if candidate != "" {
			return candidate
		}
	}
	return "localhost"
}

func hostnameOnly(host string) string {
	h := host
	if strings.HasPrefix(h, "[") {
		if end := strings.Index(h, "]"); end != -1 {
			h = h[1:end]
		}
	}
	if idx := strings.LastIndex(h, ":"); idx != -1 && idx > strings.Index(h, "]") {
		h = h[:idx]
	}
	return h
}

var _ Provider = (*xrpcsubscription.Provider)(nil)

var _ Provider = (*iroh.Provider)(nil)

var _ Provider = (*opentunnel.Provider)(nil)

package ingressproxy

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/iroh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/opentunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/xrpcsubscription"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
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
	RelayRef          *commonv1.ObjectRef
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
	workspace = cmp.Or(workspace, "root")
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
		cfg.PLCURL = strings.TrimRight(strings.TrimSpace(decoded.Network.PLCDirectoryURL), "/")
		if cfg.IngressProxyHost == "" {
			cfg.IngressProxyHost = strings.TrimSpace(decoded.Network.IngressProxyHost)
		}
		if cfg.Hostname == "" {
			cfg.Hostname = hostport.HostnameOnly(strings.TrimSpace(decoded.Network.IngressProxyHost))
		}
		if cfg.RelayURL == "" {
			cfg.RelayURL = decoded.Network.RelayURL()
		}
	}
	if cfg.Hostname == "" {
		cfg.Hostname = hostport.HostnameOnly(cfg.IngressProxyHost)
	}
	if cfg.Hostname == "" && len(cfg.Hostnames) > 0 {
		cfg.Hostname = hostport.HostnameOnly(strings.TrimSpace(cfg.Hostnames[0]))
	}
	if cfg.Hostname == "" && decoded.Identity != nil {
		cfg.Hostname = hostport.HostnameOnly(strings.TrimSpace(decoded.Identity.Handle))
	}
	if decoded.Identity != nil && strings.TrimSpace(decoded.Identity.Did) != "" {
		cfg.AllowedDIDs = strs.AppendUniqueFold(cfg.AllowedDIDs, decoded.Identity.Did)
	}
	if host := hostport.HostnameOnly(cfg.IngressProxyHost); host != "" {
		cfg.AllowedHosts = strs.AppendUniqueFold(cfg.AllowedHosts, host, "*."+host)
	}
	if host := hostport.HostnameOnly(cfg.PublicHost); host != "" {
		cfg.AllowedHosts = strs.AppendUniqueFold(cfg.AllowedHosts, host)
		cfg.Hostnames = strs.AppendUniqueFold(cfg.Hostnames, host)
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
		cfg.Hostnames = strs.AppendUniqueFold(cfg.Hostnames, decoded.TLS.Hostnames...)
	}
	if decoded.Network != nil {
		cfg.Hostnames = strs.AppendUniqueFold(cfg.Hostnames, hostport.NetworkHostnames(decoded.Network, cfg.Port)...)
	}
	cfg.AdditionalHosts = strs.AppendUniqueFold(cfg.AdditionalHosts, additionalHosts(hostport.Primary(cfg.Hostname, cfg.Hostnames), cfg.Hostnames)...)
	if wildcard := wildcardHost(cfg.Hostnames); wildcard != "" {
		cfg.AllowedHosts = strs.AppendUniqueFold(cfg.AllowedHosts, wildcard)
	}
	if decoded.RelayRef != nil {
		if relayURL, ok := relayEndpoint(cfg.Deps, cfg.Workspace, decoded.RelayRef); ok {
			cfg.RelayURL = relayURL
		}
	}
	if decoded.TLS != nil && decoded.TLS.Enabled {
		certFile, keyFile := servetls.CertificateFiles(decoded.TLS.CertRef, decoded.Storage, cfg.Hostnames)
		cfg.CertFile = certFile
		cfg.KeyFile = keyFile
		tlsConfig, err := servetls.Config(cfg.CertFile, deps.TLS, deps.Log)
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

func additionalHosts(primary string, hostnames []string) []string {
	wanted := strings.ToLower(hostport.HostnameOnly(primary))
	var out []string
	for _, host := range hostnames {
		if strings.ToLower(hostport.HostnameOnly(host)) == wanted {
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

func relayEndpoint(deps component.Deps, workspace string, ref *commonv1.ObjectRef) (string, bool) {
	if ref == nil {
		return "", false
	}
	kind := strings.TrimSpace(ref.Kind)
	kind = cmp.Or(kind, "Relay")
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
				return hostport.EndpointURL(endpoint)
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
				return hostport.EndpointURL(endpoint)
			}
		}
	}
	return "", false
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
	cfg := s.cfg
	cfg.Deps = cfg.Deps.Or(deps)
	cfg.Workspace = cmp.Or(cfg.Workspace, "root")
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
		return "https://" + hostport.HostnameOnly(value)
	}
	host := hostport.HostnameOnly(hostport.Primary(s.cfg.Hostname, s.cfg.Hostnames))
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

var _ Provider = (*xrpcsubscription.Provider)(nil)

var _ Provider = (*iroh.Provider)(nil)

var _ Provider = (*opentunnel.Provider)(nil)

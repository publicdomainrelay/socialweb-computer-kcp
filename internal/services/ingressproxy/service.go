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

	"k8s.io/utils/ptr"

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

type Deps struct {
	component.Instance
	Registry *component.Registry
	TLS      *tls.Config
	Identity exposure.IngressIdentity
}

type Service struct {
	spec v1alpha1.IngressProxySpec
	deps Deps

	workspace        string
	ingressProxyHost string
	publicHost       string
	hostname         string
	hostnames        []string
	relayURL         string
	plcURL           string
	additionalHosts  []string
	allowedDIDs      []string
	allowedHosts     []string
	tlsConfig        *tls.Config

	mu sync.Mutex

	provider Provider

	providerName string
}

func New(spec v1alpha1.IngressProxySpec, deps Deps) (*Service, error) {
	s := &Service{
		spec:             spec,
		deps:             deps,
		workspace:        cmp.Or(strings.TrimSpace(deps.Self.Workspace), "root"),
		ingressProxyHost: strings.TrimSpace(spec.IngressProxyHost),
		publicHost:       strings.TrimSpace(spec.PublicHost),
		hostnames:        append([]string(nil), spec.Hostnames...),
		relayURL:         strings.TrimSpace(spec.RelayURL),
	}
	if spec.Network != nil {
		s.plcURL = strings.TrimRight(strings.TrimSpace(spec.Network.PLCDirectoryURL), "/")
		if s.ingressProxyHost == "" {
			s.ingressProxyHost = strings.TrimSpace(spec.Network.IngressProxyHost)
		}
		s.hostname = hostport.HostnameOnly(strings.TrimSpace(spec.Network.IngressProxyHost))
		if s.relayURL == "" {
			s.relayURL = spec.Network.RelayURL()
		}
	}
	if s.hostname == "" {
		s.hostname = hostport.HostnameOnly(s.ingressProxyHost)
	}
	if s.hostname == "" && len(s.hostnames) > 0 {
		s.hostname = hostport.HostnameOnly(strings.TrimSpace(s.hostnames[0]))
	}
	if s.hostname == "" && spec.Identity != nil {
		s.hostname = hostport.HostnameOnly(strings.TrimSpace(spec.Identity.Handle))
	}
	if spec.Identity != nil && strings.TrimSpace(spec.Identity.Did) != "" {
		s.allowedDIDs = strs.AppendUniqueFold(s.allowedDIDs, spec.Identity.Did)
	}
	if host := hostport.HostnameOnly(s.ingressProxyHost); host != "" {
		s.allowedHosts = strs.AppendUniqueFold(s.allowedHosts, host, "*."+host)
	}
	if host := hostport.HostnameOnly(s.publicHost); host != "" {
		s.allowedHosts = strs.AppendUniqueFold(s.allowedHosts, host)
		s.hostnames = strs.AppendUniqueFold(s.hostnames, host)
	}
	if spec.TLS != nil {
		s.hostnames = strs.AppendUniqueFold(s.hostnames, spec.TLS.Hostnames...)
	}
	if spec.Network != nil {
		s.hostnames = strs.AppendUniqueFold(s.hostnames, hostport.NetworkHostnames(spec.Network, s.servePort())...)
	}
	s.additionalHosts = strs.AppendUniqueFold(nil, additionalHosts(hostport.Primary(s.hostname, s.hostnames), s.hostnames)...)
	if wildcard := wildcardHost(s.hostnames); wildcard != "" {
		s.allowedHosts = strs.AppendUniqueFold(s.allowedHosts, wildcard)
	}
	if relayURL, ok := relayEndpoint(deps.Registry, s.workspace, spec.RelayRef); ok {
		s.relayURL = relayURL
	}
	if spec.TLS != nil && spec.TLS.Enabled {
		certFile, _ := servetls.CertificateFiles(spec.TLS.CertRef, spec.Storage, s.hostnames)
		tlsConfig, err := servetls.Config(certFile, deps.TLS, deps.Log)
		if err != nil {
			return nil, fmt.Errorf("ingressproxy: %w", err)
		}
		s.tlsConfig = tlsConfig
	}
	return s, nil
}

func (s *Service) servePort() int {
	if s.spec.Serve == nil {
		return 0
	}
	return int(s.spec.Serve.Port)
}

func (s *Service) serveHost() string {
	if s.spec.Serve == nil {
		return ""
	}
	return s.spec.Serve.Host
}

func milliseconds(v *int32) time.Duration {
	return time.Duration(ptr.Deref(v, 0)) * time.Millisecond
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

func relayEndpoint(registry *component.Registry, workspace string, ref *commonv1.ObjectRef) (string, bool) {
	if ref == nil {
		return "", false
	}
	kind := strings.TrimSpace(ref.Kind)
	kind = cmp.Or(kind, "Relay")
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", false
	}
	if registry == nil {
		return "", false
	}
	if workspace != "" {
		for _, wanted := range []string{"http", "relay", "subscribeRepos"} {
			if endpoint, ok := registry.Endpoint(workspace, kind, name, wanted); ok {
				return hostport.EndpointURL(endpoint)
			}
		}
	}
	for _, entry := range registry.List("") {
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

func (s *Service) buildProvider(relayURL string) (Provider, string, error) {
	name := strings.ToLower(strings.TrimSpace(s.spec.Provider))
	if name == "" {
		switch {
		case s.spec.OpenTunnel != nil:
			name = v1alpha1.IngressProviderOpenTunnel
		case s.ingressProxyHost != "":
			name = v1alpha1.IngressProviderXRPCSubscription
		default:
			return nil, "", errors.New("ingressproxy: no provider selected: set spec.provider, spec.xrpc.ingressProxyHost or spec.opentunnel")
		}
	}
	switch name {
	case v1alpha1.IngressProviderXRPCSubscription:
		provider := xrpcsubscription.New(xrpcsubscription.Config{
			Host:              s.serveHost(),
			Port:              s.servePort(),
			Hostname:          s.hostname,
			Hostnames:         append([]string(nil), s.hostnames...),
			AdditionalHosts:   append([]string(nil), s.additionalHosts...),
			AllowedDIDs:       append([]string(nil), s.allowedDIDs...),
			AllowedHosts:      append([]string(nil), s.allowedHosts...),
			PLCURL:            s.plcURL,
			TLS:               s.tlsConfig,
			RelayURL:          relayURL,
			RelayTimeout:      milliseconds(s.spec.RelayTimeoutMilliseconds),
			KeepaliveInterval: milliseconds(s.spec.KeepaliveMilliseconds),
			KeepaliveTimeout:  milliseconds(s.spec.KeepaliveTimeoutMilliseconds),
			ReconnectGrace:    milliseconds(s.spec.ReconnectGraceMilliseconds),
			NonceTTL:          milliseconds(s.spec.NonceTTLMilliseconds),
			Identity:          s.deps.Identity,
			Log:               s.deps.Log,
			Registry:          s.deps.Registry,
			Bus:               s.deps.Bus,
		})
		return provider, v1alpha1.IngressProviderXRPCSubscription, nil
	case v1alpha1.IngressProviderIroh:
		return nil, "", errors.New("ingressproxy: provider iroh is not supported yet; select xrpc-subscription or opentunnel")
	case v1alpha1.IngressProviderOpenTunnel:
		spec := s.spec.OpenTunnel
		if spec == nil {
			return nil, "", errors.New("ingressproxy: provider opentunnel requires spec.opentunnel")
		}
		providerCfg := opentunnel.Config{
			Command:       append([]string(nil), spec.Command...),
			RouteName:     strings.TrimSpace(spec.RouteName),
			ProxyProtocol: strings.TrimSpace(spec.ProxyProtocol),
			LocalHost:     strings.TrimSpace(spec.LocalHost),
			StateDir:      s.deps.StateDir,
			Log:           s.deps.Log,
			Bus:           s.deps.Bus,
		}
		if err := providerCfg.Check(); err != nil {
			return nil, "", err
		}
		return opentunnel.New(providerCfg), v1alpha1.IngressProviderOpenTunnel, nil
	default:
		return nil, "", fmt.Errorf("ingressproxy: unknown provider %q (use %s, %s or %s)", s.spec.Provider,
			v1alpha1.IngressProviderXRPCSubscription, v1alpha1.IngressProviderIroh, v1alpha1.IngressProviderOpenTunnel)
	}
}

func (s *Service) Start(ctx context.Context) error {
	relayURL := s.relayURL
	if target, ok := relayEndpoint(s.deps.Registry, s.workspace, s.spec.RelayRef); ok {
		relayURL = target
	}
	provider, name, err := s.buildProvider(relayURL)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.provider = provider
	s.providerName = name
	s.mu.Unlock()
	return provider.Start(ctx)
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
	if value := s.publicHost; value != "" {
		if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
			return parsed.Scheme + "://" + parsed.Host
		}
		return "https://" + hostport.HostnameOnly(value)
	}
	host := hostport.HostnameOnly(hostport.Primary(s.hostname, s.hostnames))
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

var _ component.Service = (*Service)(nil)

var _ Provider = (*xrpcsubscription.Provider)(nil)

var _ Provider = (*iroh.Provider)(nil)

var _ Provider = (*opentunnel.Provider)(nil)

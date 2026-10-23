package relay

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "Relay"

type Deps struct {
	component.Instance
	Registry *component.Registry
	TLS      *tls.Config
	Egress   component.Egress
}

type Service struct {
	spec v1alpha1.RelaySpec
	deps Deps

	workspace     string
	serveHost     string
	hostnames     []string
	plcURL        string
	insecureHTTP  bool
	allowedHosts  []string
	crawlInterval time.Duration
	tlsConfig     *tls.Config

	mu       sync.Mutex
	log      *slog.Logger
	relay    *atrelay.Relay
	store    atrelay.Store
	key      component.Key
	baseURL  string
	stopCh   chan struct{}
	stopOnce sync.Once
	ready    bool
}

const defaultCrawlInterval = 30 * time.Second

func New(spec v1alpha1.RelaySpec, deps Deps) (*Service, error) {
	s := &Service{
		spec:          spec,
		deps:          deps,
		workspace:     cmp.Or(strings.TrimSpace(deps.Self.Workspace), "root"),
		hostnames:     append([]string(nil), spec.Hostnames...),
		crawlInterval: time.Duration(ptr.Deref(spec.CrawlIntervalSeconds, 0)) * time.Second,
	}
	if s.crawlInterval <= 0 && len(spec.PdsRefs) > 0 {
		s.crawlInterval = defaultCrawlInterval
	}
	port := 0
	if spec.Serve != nil {
		s.serveHost = spec.Serve.Host
		port = int(spec.Serve.Port)
	}
	if spec.Network != nil {
		s.plcURL = strings.TrimRight(strings.TrimSpace(spec.Network.PLCDirectoryURL), "/")
		s.insecureHTTP = strings.HasPrefix(strings.ToLower(s.plcURL), "http://")
	}
	if spec.TLS != nil {
		s.hostnames = strs.AppendUniqueFold(s.hostnames, spec.TLS.Hostnames...)
	}
	if spec.Network != nil {
		s.hostnames = strs.AppendUniqueFold(s.hostnames, hostport.NetworkHostnames(spec.Network, port)...)
	}
	upstreams := pdsEndpoints(deps.Registry, s.workspace, spec.PdsRefs)
	s.allowedHosts = resolvedHosts(upstreams, spec.PdsRefs)
	for _, endpoint := range upstreams {
		if strings.HasPrefix(strings.ToLower(endpoint.URL), "http://") {
			s.insecureHTTP = true
		}
	}
	if spec.TLS != nil && spec.TLS.Enabled {
		certFile, _ := servetls.CertificateFiles(spec.TLS.CertRef, spec.Storage, s.hostnames)
		tlsConfig, err := servetls.Config(certFile, deps.TLS, deps.Log)
		if err != nil {
			return nil, fmt.Errorf("relay: %w", err)
		}
		s.tlsConfig = tlsConfig
	}
	return s, nil
}

var _ component.Service = (*Service)(nil)

func (s *Service) stateDir() string {
	if s.spec.Storage != nil && s.spec.Storage.DataDir != "" {
		return s.spec.Storage.DataDir
	}
	return s.deps.StateDir
}

func pdsEndpoints(registry *component.Registry, workspace string, refs []commonv1.ObjectRef) []component.Endpoint {
	out := make([]component.Endpoint, 0, len(refs))
	for _, ref := range refs {
		kind := strings.TrimSpace(ref.Kind)
		kind = cmp.Or(kind, "Pds")
		if ref.Name == "*" {
			if registry == nil {
				continue
			}
			for _, entry := range registry.List("") {
				if entry.Key.Kind != kind {
					continue
				}
				if endpoint, ok := component.PrimaryEndpoint(entry.Endpoints); ok {
					out = append(out, endpoint)
				}
			}
			continue
		}
		if endpoint, ok := resolveEndpoint(registry, workspace, kind, ref.Name, "http", "xrpc", "pds"); ok {
			out = append(out, endpoint)
		}
	}
	return out
}

func allowsAnyPDS(refs []commonv1.ObjectRef) bool {
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "*" {
			return true
		}
	}
	return false
}

func resolvedHosts(endpoints []component.Endpoint, refs []commonv1.ObjectRef) []string {
	if len(endpoints) != len(refs) {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		host := endpointHost(endpoint)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

func resolveEndpoint(registry *component.Registry, workspace, kind, name string, names ...string) (component.Endpoint, bool) {
	if registry == nil || kind == "" || name == "" {
		return component.Endpoint{}, false
	}
	if workspace != "" {
		for _, wanted := range names {
			if endpoint, ok := registry.Endpoint(workspace, kind, name, wanted); ok {
				return endpoint, true
			}
		}
	}
	for _, entry := range registry.List("") {
		if entry.Key.Kind != kind || entry.Key.Name != name {
			continue
		}
		for _, wanted := range names {
			for _, endpoint := range entry.Endpoints {
				if endpoint.Name == wanted {
					return endpoint, true
				}
			}
		}
		for _, endpoint := range entry.Endpoints {
			if endpoint.Name != "" {
				return endpoint, true
			}
		}
	}
	return component.Endpoint{}, false
}

func (s *Service) Start(ctx context.Context) error {
	upstreams := pdsEndpoints(s.deps.Registry, s.workspace, s.spec.PdsRefs)
	allowedHosts := s.allowedHosts
	if len(allowedHosts) == 0 {
		allowedHosts = resolvedHosts(upstreams, s.spec.PdsRefs)
	}
	insecureHTTP := s.insecureHTTP
	for _, endpoint := range upstreams {
		if strings.HasPrefix(strings.ToLower(endpoint.URL), "http://") {
			insecureHTTP = true
		}
	}
	log := s.deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stateDir := s.stateDir()
	var store atrelay.Store = atrelay.NewMemoryStore()
	var persistent *fileStore
	if stateDir != "" {
		fs, err := openFileStore(filepath.Join(stateDir, "relay-state.json"))
		if err != nil {
			return fmt.Errorf("relay: opening the crawl state: %w", err)
		}
		store = fs
		persistent = fs
	}
	tlsConfig := s.deps.TLS
	if tlsConfig == nil {
		tlsConfig = s.tlsConfig
	}
	handlers := s.deps.Bus
	httpClient := handlerClient(handlers, s.deps.Egress.Client(0).Transport)
	dialContext := handlerDialer(handlers, externalDialContext(s.deps.Egress))
	var directory identity.Directory
	if s.plcURL != "" {
		base := &identity.BaseDirectory{
			PLCURL:                 strings.TrimSuffix(s.plcURL, "/"),
			HTTPClient:             *httpClient,
			PLCClient:              httpClient,
			SkipHandleVerification: true,
		}
		directory = identity.NewCacheDirectory(base, 100_000, time.Hour, 2*time.Minute, 5*time.Minute)
	}
	relay := atrelay.New(atrelay.Config{
		Hostname:     hostport.Primary(s.serveHost, s.hostnames),
		Logger:       log,
		Store:        store,
		Directory:    directory,
		PLCURL:       s.plcURL,
		InsecureHTTP: insecureHTTP,
		AllowedHosts: append([]string(nil), allowedHosts...),
		HTTPClient:   httpClient,
		DialContext:  dialContext,
		TLSConfig:    tlsConfig,
	})
	key := s.deps.Key()
	key.Kind = cmp.Or(key.Kind, Kind)
	key.Name = cmp.Or(key.Name, "default")
	if handlers == nil {
		relay.Close()
		return errors.New("relay: no handler registry is available")
	}
	stopCh := make(chan struct{})
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		relay.Close()
		return errors.New("relay: the service already started")
	}
	s.stopCh = stopCh
	s.mu.Unlock()
	host := kcp.KindServiceHost(key.Kind, key.Workspace)
	baseURL := "http://" + host
	if err := handlers.RegisterHTTP(key, relay.Handler()); err != nil {
		s.mu.Lock()
		s.stopCh = nil
		s.mu.Unlock()
		relay.Close()
		return fmt.Errorf("relay: registering the handler: %w", err)
	}

	s.mu.Lock()
	s.log = log
	s.relay = relay
	s.store = store
	s.key = key
	s.baseURL = baseURL
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("relay_registered", "host", host, "hostname", hostport.Primary(s.serveHost, s.hostnames))

	flushDone := make(chan struct{})
	flushStopped := make(chan struct{})
	defer func() {
		close(flushDone)
		<-flushStopped
	}()
	if persistent != nil {
		go persistent.run(time.Second, flushDone, flushStopped)
	} else {
		close(flushStopped)
	}
	crawlCtx, crawlCancel := context.WithCancel(ctx)
	defer crawlCancel()
	var crawls sync.WaitGroup
	crawls.Go(func() {
		crawl(crawlCtx, log, relay, func() []string { return s.crawlHosts(upstreams) }, s.crawlInterval, stopCh)
	})

	select {
	case <-ctx.Done():
	case <-stopCh:
	}
	crawlCancel()
	crawls.Wait()

	relay.Close()
	s.finish()
	return nil
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	ready := s.ready
	baseURL := s.baseURL
	handlers := s.deps.Bus
	s.mu.Unlock()
	if !ready || baseURL == "" || handlers == nil {
		return errors.New("relay: the service is not registered")
	}
	if err := xrpc.Health(ctx, &xrpc.Client{Host: baseURL, HTTP: handlers.HTTPClient()}); err != nil {
		return fmt.Errorf("relay: health: %w", err)
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.baseURL == "" {
		return nil
	}
	return []component.Endpoint{
		{Name: "http", Protocol: component.ProtocolHTTP, URL: s.baseURL},
		{Name: "subscribeRepos", Protocol: component.ProtocolWS, URL: "ws" + strings.TrimPrefix(s.baseURL, "http") + "/xrpc/com.atproto.sync.subscribeRepos"},
	}
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	stopCh := s.stopCh
	s.mu.Unlock()
	if stopCh == nil {
		return nil
	}
	s.stopOnce.Do(func() { close(stopCh) })
	return nil
}

func (s *Service) Relay() *atrelay.Relay {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.relay
}

func (s *Service) Store() atrelay.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store
}

func (s *Service) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseURL == "" {
		return ""
	}
	return strings.TrimPrefix(s.baseURL, "http://")
}

func (s *Service) AddSink(sink atrelay.EventSink) {
	s.mu.Lock()
	relay := s.relay
	s.mu.Unlock()
	if relay != nil {
		relay.AddSink(sink)
	}
}

func (s *Service) finish() {
	s.mu.Lock()
	s.ready = false
	s.stopCh = nil
	s.mu.Unlock()
}

func (s *Service) crawlHosts(upstreams []component.Endpoint) []string {
	if !allowsAnyPDS(s.spec.PdsRefs) || s.deps.Registry == nil {
		return crawlHosts(s.deps.Registry, s.workspace, s.spec.PdsRefs, upstreams)
	}
	eps := pdsEndpoints(s.deps.Registry, s.workspace, []commonv1.ObjectRef{{Kind: "Pds", Name: "*"}})
	out := make([]string, 0, len(eps))
	seen := map[string]struct{}{}
	for _, endpoint := range eps {
		host := endpointHost(endpoint)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

func crawl(ctx context.Context, log *slog.Logger, relay *atrelay.Relay, hosts func() []string, interval time.Duration, stop <-chan struct{}) {
	request := func() {
		for _, host := range hosts() {
			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := relay.RequestCrawl(reqCtx, host)
			cancel()
			if err != nil {
				log.Warn("relay_crawl_failed", "hostname", host, "err", err)
				continue
			}
			log.Info("relay_crawl_started", "hostname", host)
		}
	}
	request()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			request()
		}
	}
}

func crawlHosts(registry *component.Registry, workspace string, refs []commonv1.ObjectRef, upstreams []component.Endpoint) []string {
	seen := map[string]struct{}{}
	out := []string{}
	add := func(host string) {
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	for _, endpoint := range upstreams {
		add(endpointHost(endpoint))
	}
	for _, endpoint := range pdsEndpoints(registry, workspace, refs) {
		add(endpointHost(endpoint))
	}
	return out
}

func endpointHost(endpoint component.Endpoint) string {
	if endpoint.URL != "" {
		parsed, err := url.Parse(endpoint.URL)
		if err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return endpoint.Address
}

func handlerClient(handlers *bus.Bus, external http.RoundTripper) *http.Client {
	return &http.Client{Transport: handlers.Transport(external)}
}

func externalDialContext(egress component.Egress) func(context.Context, string, string) (net.Conn, error) {
	dialer, err := networkdial.NewDialerWithOptions(networkdial.DialOptions{
		Hosts:          egress.Hosts,
		TLSConfig:      egress.TLS,
		AllowSystemDNS: true,
	})
	if err != nil {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	return dialer.DialContext
}

func handlerDialer(handlers *bus.Bus, external func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if handlers != nil {
			if _, ok := handlers.HandlerForHost(address); ok {
				return handlers.DialHTTP(ctx, address)
			}
		}
		return external(ctx, network, address)
	}
}

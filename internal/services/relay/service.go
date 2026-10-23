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

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "Relay"

const DefaultHost = "127.0.0.1"

const DefaultShutdownTimeout = 5 * time.Second

const DefaultCrawlInterval = 30 * time.Second

type Config struct {
	Deps            component.Deps
	Key             component.Key
	StateDir        string
	Workspace       string
	Address         string
	Host            string
	Port            int
	Hostnames       []string
	Network         *v1alpha1.MarketNetworkValue
	PdsRefs         []commonv1.ObjectRef
	Version         string
	PLCURL          string
	AllowedDIDs     []string
	AllowedHosts    []string
	CrawlHosts      []string
	CrawlInterval   time.Duration
	Upstreams       []component.Endpoint
	Sinks           []atrelay.EventSink
	Store           atrelay.Store
	Directory       identity.Directory
	InsecureHTTP    bool
	DisableVerify   bool
	StrictVerify    bool
	MaxBacklog      int
	CertFile        string
	KeyFile         string
	TLSConfig       *tls.Config
	HTTPClient      *http.Client
	DialContext     func(ctx context.Context, network, address string) (net.Conn, error)
	ShutdownTimeout time.Duration
}

type Service struct {
	cfg Config

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

func New(cfg Config) *Service {
	return &Service{cfg: cfg}
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.RelaySpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("relay: decoding the component spec: %w", err)
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	workspace := strings.TrimSpace(spec.Workspace)
	workspace = cmp.Or(workspace, "root")
	cfg := Config{
		Deps:      deps,
		Key:       component.Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name},
		StateDir:  stateDir,
		Workspace: workspace,
		Host:      DefaultHost,
		Hostnames: append([]string(nil), decoded.Hostnames...),
		PdsRefs:   append([]commonv1.ObjectRef(nil), decoded.PdsRefs...),
	}
	if decoded.Network != nil {
		network := *decoded.Network
		cfg.Network = &network
		cfg.PLCURL = strings.TrimRight(strings.TrimSpace(decoded.Network.PLCDirectoryURL), "/")
		cfg.InsecureHTTP = strings.HasPrefix(strings.ToLower(cfg.PLCURL), "http://")
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
	cfg.Upstreams = pdsEndpoints(cfg.Deps, cfg.Workspace, cfg.PdsRefs)
	if allowsAnyPDS(cfg.PdsRefs) {
		cfg.AllowedDIDs = nil
	}
	cfg.AllowedHosts = resolvedHosts(cfg.Upstreams, cfg.PdsRefs)
	for _, endpoint := range cfg.Upstreams {
		if strings.HasPrefix(strings.ToLower(endpoint.URL), "http://") {
			cfg.InsecureHTTP = true
		}
	}
	if decoded.CrawlIntervalSeconds != nil && *decoded.CrawlIntervalSeconds > 0 {
		cfg.CrawlInterval = time.Duration(*decoded.CrawlIntervalSeconds) * time.Second
	}
	if cfg.CrawlInterval <= 0 && len(cfg.PdsRefs) > 0 {
		cfg.CrawlInterval = DefaultCrawlInterval
	}
	if decoded.TLS != nil && decoded.TLS.Enabled {
		certFile, keyFile := servetls.CertificateFiles(decoded.TLS.CertRef, decoded.Storage, cfg.Hostnames)
		cfg.CertFile = certFile
		cfg.KeyFile = keyFile
		tlsConfig, err := servetls.Config(cfg.CertFile, deps.TLS, deps.Log)
		if err != nil {
			return Config{}, fmt.Errorf("relay: %w", err)
		}
		cfg.TLSConfig = tlsConfig
	}
	return cfg, nil
}

func pdsEndpoints(deps component.Deps, workspace string, refs []commonv1.ObjectRef) []component.Endpoint {
	out := make([]component.Endpoint, 0, len(refs))
	for _, ref := range refs {
		kind := strings.TrimSpace(ref.Kind)
		kind = cmp.Or(kind, "Pds")
		if ref.Name == "*" {
			if deps.Registry == nil {
				continue
			}
			for _, entry := range deps.Registry.List("") {
				if entry.Key.Kind != kind {
					continue
				}
				if endpoint, ok := component.PrimaryEndpoint(entry.Endpoints); ok {
					out = append(out, endpoint)
				}
			}
			continue
		}
		if endpoint, ok := resolveEndpoint(deps, workspace, kind, ref.Name, "http", "xrpc", "pds"); ok {
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

func resolveEndpoint(deps component.Deps, workspace, kind, name string, names ...string) (component.Endpoint, bool) {
	if deps.Registry == nil || kind == "" || name == "" {
		return component.Endpoint{}, false
	}
	if workspace != "" {
		for _, wanted := range names {
			if endpoint, ok := deps.Registry.Endpoint(workspace, kind, name, wanted); ok {
				return endpoint, true
			}
		}
	}
	for _, entry := range deps.Registry.List("") {
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

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	cfg := s.cfg
	cfg.Deps = cfg.Deps.Or(deps)
	cfg.Workspace = cmp.Or(cfg.Workspace, "root")
	cfg.Upstreams = pdsEndpoints(cfg.Deps, cfg.Workspace, cfg.PdsRefs)
	if len(cfg.AllowedHosts) == 0 {
		cfg.AllowedHosts = resolvedHosts(cfg.Upstreams, cfg.PdsRefs)
	}
	if allowsAnyPDS(cfg.PdsRefs) {
		cfg.AllowedDIDs = nil
	}
	for _, endpoint := range cfg.Upstreams {
		if strings.HasPrefix(strings.ToLower(endpoint.URL), "http://") {
			cfg.InsecureHTTP = true
		}
	}
	log := cfg.Deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stateDir := cfg.StateDir
	stateDir = cmp.Or(stateDir, cfg.Deps.StateDir)
	store := cfg.Store
	var persistent *fileStore
	if store == nil {
		store = atrelay.NewMemoryStore()
		if stateDir != "" {
			fs, err := openFileStore(filepath.Join(stateDir, "relay-state.json"))
			if err != nil {
				return fmt.Errorf("relay: opening the crawl state: %w", err)
			}
			store = fs
			persistent = fs
		}
	}
	tlsConfig := cfg.Deps.TLS
	if tlsConfig == nil {
		tlsConfig = cfg.TLSConfig
	}
	handlers := cfg.Deps.Handlers
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = handlerClient(handlers, cfg.Deps.ExternalHTTPClient(0).Transport)
	}
	if cfg.DialContext == nil {
		cfg.DialContext = handlerDialer(handlers, externalDialContext(cfg.Deps))
	}
	if cfg.Directory == nil && cfg.PLCURL != "" {
		base := &identity.BaseDirectory{
			PLCURL:                 strings.TrimSuffix(cfg.PLCURL, "/"),
			HTTPClient:             *cfg.HTTPClient,
			PLCClient:              cfg.HTTPClient,
			SkipHandleVerification: true,
		}
		cfg.Directory = identity.NewCacheDirectory(base, 100_000, time.Hour, 2*time.Minute, 5*time.Minute)
	}
	relay := atrelay.New(atrelay.Config{
		Hostname:      hostport.Primary(cfg.Host, cfg.Hostnames),
		Version:       cfg.Version,
		Logger:        log,
		Store:         store,
		Directory:     cfg.Directory,
		PLCURL:        cfg.PLCURL,
		InsecureHTTP:  cfg.InsecureHTTP,
		DisableVerify: cfg.DisableVerify,
		StrictVerify:  cfg.StrictVerify,
		MaxBacklog:    cfg.MaxBacklog,
		AllowedDIDs:   append([]string(nil), cfg.AllowedDIDs...),
		AllowedHosts:  append([]string(nil), cfg.AllowedHosts...),
		HTTPClient:    cfg.HTTPClient,
		DialContext:   cfg.DialContext,
		TLSConfig:     tlsConfig,
	})
	for _, sink := range cfg.Sinks {
		relay.AddSink(sink)
	}
	key := cfg.Key
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
	s.cfg = cfg
	s.log = log
	s.relay = relay
	s.store = store
	s.key = key
	s.baseURL = baseURL
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("relay_registered", "host", host, "hostname", hostport.Primary(cfg.Host, cfg.Hostnames))

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
		crawl(crawlCtx, log, relay, func() []string { return s.crawlHosts(cfg) }, cfg.CrawlInterval, stopCh)
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
	handlers := s.cfg.Deps.Handlers
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

func (s *Service) crawlHosts(cfg Config) []string {
	if !allowsAnyPDS(cfg.PdsRefs) || cfg.Deps.Registry == nil {
		return crawlHosts(cfg)
	}
	eps := pdsEndpoints(cfg.Deps, cfg.Workspace, []commonv1.ObjectRef{{Kind: "Pds", Name: "*"}})
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

func crawlHosts(cfg Config) []string {
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
	for _, host := range cfg.CrawlHosts {
		add(host)
	}
	for _, endpoint := range cfg.Upstreams {
		add(endpointHost(endpoint))
	}
	for _, endpoint := range pdsEndpoints(cfg.Deps, cfg.Workspace, cfg.PdsRefs) {
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

func handlerClient(handlers *component.HandlerRegistry, external http.RoundTripper) *http.Client {
	return &http.Client{Transport: handlers.Transport(external)}
}

func externalDialContext(deps component.Deps) func(context.Context, string, string) (net.Conn, error) {
	dialer, err := networkdial.NewDialerWithOptions(networkdial.DialOptions{
		Hosts:          deps.Hosts,
		TLSConfig:      deps.TLS,
		AllowSystemDNS: true,
	})
	if err != nil {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	return dialer.DialContext
}

func handlerDialer(handlers *component.HandlerRegistry, external func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if handlers != nil {
			if _, ok := handlers.HandlerForHost(address); ok {
				return handlers.DialHTTP(ctx, address)
			}
		}
		return external(ctx, network, address)
	}
}

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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/relay/local"
)

const (
	Kind = "Relay"

	PdsKind = "Pds"

	AnyName = "*"
)

type Deps struct {
	component.Instance
	Registry *component.Registry
	TLS      *tls.Config
	Egress   component.Egress
}

type Service struct {
	spec v1alpha1.RelaySpec
	deps Deps

	serveHost     string
	hostnames     []string
	plcURL        string
	insecureHTTP  bool
	allowedHosts  []string
	crawlInterval time.Duration
	tlsConfig     *tls.Config

	mu       sync.Mutex
	relay    *local.Relay
	key      component.Key
	baseURL  string
	stopCh   chan struct{}
	stopOnce sync.Once
	ready    bool

	readiness component.ReadySignal
}

func New(spec v1alpha1.RelaySpec, deps Deps) (*Service, error) {
	s := &Service{
		spec:          spec,
		deps:          deps,
		hostnames:     append([]string(nil), spec.Hostnames...),
		crawlInterval: time.Duration(ptr.Deref(spec.CrawlIntervalSeconds, 0)) * time.Second,
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
	if len(spec.PdsRefs) > 0 {
		s.insecureHTTP = true
	}
	if !s.anyPDS() {
		for _, ref := range spec.PdsRefs {
			s.allowedHosts = strs.AppendUniqueFold(s.allowedHosts, pdsHost(refKind(ref.Kind), ref.Name))
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

func refKind(kind string) string {
	return cmp.Or(strings.TrimSpace(kind), PdsKind)
}

func pdsHost(kind, name string) string {
	kind, name = hostport.Normalize(kind), hostport.Normalize(name)
	if kind == "" || name == "" || name == AnyName {
		return ""
	}
	return hostport.Normalize(kind + "." + name)
}

func (s *Service) anyPDS() bool {
	for _, ref := range s.spec.PdsRefs {
		if strings.TrimSpace(ref.Name) == AnyName {
			return true
		}
	}
	return false
}

func (s *Service) upstream(key component.Key) (string, bool) {
	for _, ref := range s.spec.PdsRefs {
		if !strings.EqualFold(refKind(ref.Kind), key.Kind) {
			continue
		}
		name := strings.TrimSpace(ref.Name)
		if name != AnyName && !strings.EqualFold(name, key.Name) {
			continue
		}
		if host := pdsHost(key.Kind, key.Name); host != "" {
			return host, true
		}
	}
	return "", false
}

func (s *Service) observe(crawler contract.Crawler, key component.Key) {
	host, ok := s.upstream(key)
	if !ok {
		return
	}
	if _, ok := s.deps.Bus.HandlerForHost(host); ok {
		crawler.Crawl(host)
		return
	}
	crawler.Forget(host)
}

func (s *Service) follow(crawler contract.Crawler) func() {
	if len(s.spec.PdsRefs) == 0 {
		return func() {}
	}
	cancel := s.deps.Registry.Subscribe(func(key component.Key) { s.observe(crawler, key) })
	for _, ref := range s.spec.PdsRefs {
		if strings.TrimSpace(ref.Name) != AnyName {
			s.observe(crawler, component.Key{Workspace: s.deps.Self.Workspace, Kind: refKind(ref.Kind), Name: strings.TrimSpace(ref.Name)})
		}
	}
	if s.anyPDS() {
		for _, entry := range s.deps.Registry.List("") {
			s.observe(crawler, entry.Key)
		}
	}
	return cancel
}

func (s *Service) Start(ctx context.Context) error {
	ctx = s.deps.Caller(ctx)
	handlers := s.deps.Bus
	if handlers == nil {
		return errors.New("relay: no handler registry is available")
	}
	log := s.deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stateFile := ""
	if dir := s.stateDir(); dir != "" {
		stateFile = filepath.Join(dir, "relay-state.json")
	}
	tlsConfig := s.deps.TLS
	if tlsConfig == nil {
		tlsConfig = s.tlsConfig
	}
	hostname := hostport.Primary(s.serveHost, s.hostnames)
	relay, err := local.New(local.Config{
		Hostname:      hostname,
		PLCURL:        s.plcURL,
		InsecureHTTP:  s.insecureHTTP,
		AllowedHosts:  s.allowedHosts,
		StateFile:     stateFile,
		CrawlInterval: s.crawlInterval,
		TLS:           tlsConfig,
	}, local.Deps{
		Log:         log,
		HTTPClient:  &http.Client{Transport: handlers.Transport(s.deps.Egress.Client(0).Transport)},
		DialContext: handlerDialer(handlers, externalDialContext(s.deps.Egress)),
		BaseContext: ctx,
	})
	if err != nil {
		return fmt.Errorf("relay: opening the crawl state: %w", err)
	}
	key := s.deps.Key()
	key.Kind = cmp.Or(key.Kind, Kind)
	key.Name = cmp.Or(key.Name, "default")
	stopCh := make(chan struct{})
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		relay.Core().Close()
		return errors.New("relay: the service already started")
	}
	s.stopCh = stopCh
	s.mu.Unlock()
	host := kcp.KindServiceHost(key.Kind, key.Workspace)
	if err := handlers.RegisterHTTP(key, relay.Handler()); err != nil {
		s.mu.Lock()
		s.stopCh = nil
		s.mu.Unlock()
		relay.Core().Close()
		return fmt.Errorf("relay: registering the handler: %w", err)
	}

	s.mu.Lock()
	s.relay = relay
	s.key = key
	s.baseURL = "http://" + host
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("relay_registered", "host", host, "hostname", hostname)
	s.readiness.Signal()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-runCtx.Done():
		}
	}()
	unfollow := s.follow(relay)
	_ = relay.Run(runCtx)
	unfollow()
	s.finish()
	return nil
}

func (s *Service) Ready() component.Readiness {
	return &s.readiness
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
	if s.relay == nil {
		return nil
	}
	return s.relay.Core()
}

func (s *Service) Store() atrelay.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.relay == nil {
		return nil
	}
	return s.relay.Store()
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
	if relay := s.Relay(); relay != nil {
		relay.AddSink(sink)
	}
}

func (s *Service) finish() {
	s.mu.Lock()
	s.ready = false
	s.stopCh = nil
	s.mu.Unlock()
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
		if _, ok := handlers.HandlerForHost(address); ok {
			return handlers.DialHTTP(ctx, address)
		}
		return external(ctx, network, address)
	}
}

package dispatchertls

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "DispatcherTLS"

const DefaultHost = "127.0.0.1"

const DefaultShutdownTimeout = 5 * time.Second

type Config struct {
	Deps            component.Deps
	StateDir        string
	Workspace       string
	Address         string
	Host            string
	Port            int
	Hostnames       []string
	AllowedHosts    []string
	UpstreamRef     *commonv1.ObjectRef
	Upstream        component.Endpoint
	UpstreamURL     string
	UpstreamAddress string
	UpstreamTLS     *tls.Config
	TLS             *tls.Config
	CertFile        string
	KeyFile         string
	CABundle        string
	ShutdownTimeout time.Duration
}

type Service struct {
	cfg Config

	mu       sync.Mutex
	log      *slog.Logger
	http     *http.Server
	listener net.Listener
	conns    *connSet
	stopCh   chan struct{}
	stopOnce sync.Once
	ready    bool
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg}
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.DispatcherTLSSpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("dispatchertls: decoding the component spec: %w", err)
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	workspace := strings.TrimSpace(spec.Workspace)
	workspace = cmp.Or(workspace, "root")
	cfg := Config{
		Deps:        deps,
		StateDir:    stateDir,
		Workspace:   workspace,
		Host:        DefaultHost,
		Hostnames:   append([]string(nil), decoded.Hostnames...),
		UpstreamRef: decoded.UpstreamRef,
	}
	if decoded.TLS != nil {
		cfg.Hostnames = strs.AppendUniqueFold(cfg.Hostnames, decoded.TLS.Hostnames...)
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
		cfg.CABundle = strings.TrimSpace(decoded.TLS.CABundle)
		if decoded.TLS.CertRef != "" || decoded.TLS.Enabled {
			certFile, keyFile := servetls.CertificateFiles(decoded.TLS.CertRef, decoded.Storage, cfg.Hostnames)
			cfg.CertFile = certFile
			cfg.KeyFile = keyFile
		}
	}
	if decoded.UpstreamRef != nil {
		if target, ok := upstreamEndpoint(cfg.Deps, cfg.Workspace, decoded.UpstreamRef); ok {
			cfg.UpstreamURL = target
		}
	}
	return cfg, nil
}

func upstreamEndpoint(deps component.Deps, workspace string, ref *commonv1.ObjectRef) (string, bool) {
	if ref == nil || deps.Registry == nil {
		return "", false
	}
	kind := strings.TrimSpace(ref.Kind)
	kind = cmp.Or(kind, "IngressProxy")
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", false
	}
	if workspace != "" {
		for _, wanted := range []string{"http", "control"} {
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

func loadCA(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("dispatchertls: the CA bundle holds no certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("dispatchertls: parsing the CA certificate: %w", err)
		}
		pool.AddCert(cert)
	}
	return pool, nil
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	cfg := s.cfg
	cfg.Deps = cfg.Deps.Or(deps)
	cfg.Workspace = cmp.Or(cfg.Workspace, "root")
	if !upstreamConfigured(cfg) {
		return errors.New("dispatchertls: the upstream is required")
	}
	if target, ok := resolveUpstream(cfg); ok {
		cfg.UpstreamURL = target
	}
	log := cfg.Deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tlsConfig, err := serverTLS(cfg)
	if err != nil {
		return err
	}
	hosts := allowedHosts(cfg)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	upstreamTLS := cfg.UpstreamTLS
	if upstreamTLS == nil && cfg.CABundle != "" {
		pool, err := loadCA([]byte(cfg.CABundle))
		if err != nil {
			return err
		}
		upstreamTLS = &tls.Config{RootCAs: pool}
	}
	if upstreamTLS != nil {
		transport.TLSClientConfig = upstreamTLS
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			target, _ := req.In.Context().Value(upstreamTargetKey{}).(*url.URL)
			if target == nil {
				return
			}
			req.Out.URL.Scheme = target.Scheme
			req.Out.URL.Host = target.Host
			req.Out.Host = req.In.Host
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, proxyErr error) {
			log.Warn("dispatcher_tls_upstream_failed", "host", req.Host, "path", req.URL.Path, "err", proxyErr)
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := hostport.HostnameOnly(req.Host)
		if !hostAllowed(hosts, host) {
			log.Info("dispatcher_tls_refused", "host", host, "path", req.URL.Path)
			http.Error(w, "not a dispatcher name", http.StatusNotFound)
			return
		}
		raw, ok := resolveUpstream(cfg)
		if !ok {
			log.Warn("dispatcher_tls_upstream_unresolved", "host", req.Host, "path", req.URL.Path)
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		target, err := parseUpstream(raw)
		if err != nil {
			log.Warn("dispatcher_tls_upstream_invalid", "upstream", raw, "err", err)
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), upstreamTargetKey{}, target)))
	})
	listener, err := net.Listen("tcp", hostport.Listen(cfg.Address, cfg.Host, cfg.Port))
	if err != nil {
		return fmt.Errorf("dispatchertls: listening: %w", err)
	}
	tlsListener := tls.NewListener(listener, tlsConfig)
	conns := newConnSet()
	trackedListener := &trackingListener{Listener: tlsListener, set: conns}
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	stopCh := make(chan struct{})

	s.mu.Lock()
	if s.http != nil {
		s.mu.Unlock()
		_ = listener.Close()
		return errors.New("dispatchertls: the service already started")
	}
	s.cfg = cfg
	s.log = log
	s.http = httpServer
	s.listener = listener
	s.conns = conns
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("dispatcher_tls_listening", "address", listener.Addr().String(), "upstream", cfg.UpstreamURL, "hostnames", hosts)

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(trackedListener) }()

	select {
	case <-ctx.Done():
	case <-stopCh:
	case serveErr := <-errCh:
		s.finish()
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("dispatchertls: serving: %w", serveErr)
		}
		return nil
	}

	s.shutdown(ctx)
	serveErr := <-errCh
	s.finish()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("dispatchertls: serving: %w", serveErr)
	}
	return nil
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	ready := s.ready
	address := ""
	if s.listener != nil {
		address = s.listener.Addr().String()
	}
	hosts := allowedHosts(s.cfg)
	s.mu.Unlock()
	if !ready || address == "" {
		return errors.New("dispatchertls: the service is not listening")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/", nil)
	if err != nil {
		return err
	}
	req.Host = probeHost(hosts)
	resp, err := probeClient().Do(req)
	if err != nil {
		return fmt.Errorf("dispatchertls: health request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("dispatchertls: the upstream returned %d", resp.StatusCode)
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	address := s.listener.Addr().String()
	return []component.Endpoint{
		{Name: "https", Protocol: component.ProtocolHTTPS, URL: "https://" + address, Address: address},
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
	s.shutdown(ctx)
	return nil
}

func (s *Service) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Service) shutdown(ctx context.Context) {
	s.mu.Lock()
	httpServer := s.http
	conns := s.conns
	timeout := s.cfg.ShutdownTimeout
	s.mu.Unlock()
	if httpServer == nil {
		return
	}
	timeout = opt.Positive(timeout, DefaultShutdownTimeout)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		_ = httpServer.Close()
	}
	if conns != nil {
		conns.closeAll()
	}
}

func (s *Service) finish() {
	s.mu.Lock()
	s.ready = false
	s.listener = nil
	s.http = nil
	s.mu.Unlock()
}

type upstreamTargetKey struct{}

func upstreamConfigured(cfg Config) bool {
	return cfg.UpstreamURL != "" || cfg.Upstream.URL != "" || cfg.UpstreamAddress != "" ||
		cfg.Upstream.Address != "" || cfg.UpstreamRef != nil
}

func resolveUpstream(cfg Config) (string, bool) {
	if raw := strings.TrimSpace(cfg.UpstreamURL); raw != "" {
		return raw, true
	}
	if raw := strings.TrimSpace(cfg.Upstream.URL); raw != "" {
		return raw, true
	}
	if cfg.UpstreamAddress != "" {
		return "http://" + cfg.UpstreamAddress, true
	}
	if cfg.Upstream.Address != "" {
		scheme := strings.TrimSpace(cfg.Upstream.Protocol)
		scheme = cmp.Or(scheme, "http")
		return scheme + "://" + cfg.Upstream.Address, true
	}
	if cfg.UpstreamRef != nil {
		return upstreamEndpoint(cfg.Deps, cfg.Workspace, cfg.UpstreamRef)
	}
	return "", false
}

func parseUpstream(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("dispatchertls: parsing the upstream: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("dispatchertls: the upstream scheme %q is not http or https", target.Scheme)
	}
	if target.Host == "" {
		return nil, errors.New("dispatchertls: the upstream has no host")
	}
	return target, nil
}

func serverTLS(cfg Config) (*tls.Config, error) {
	if cfg.CertFile != "" {
		return servetls.Config(cfg.CertFile, nil, cfg.Deps.Log)
	}
	tlsConfig := cfg.TLS
	if tlsConfig == nil {
		tlsConfig = cfg.Deps.TLS
	}
	if tlsConfig == nil {
		return nil, errors.New("dispatchertls: a TLS config or certificate is required")
	}
	tlsConfig = tlsConfig.Clone()
	if len(tlsConfig.Certificates) == 0 && tlsConfig.GetCertificate == nil && tlsConfig.GetConfigForClient == nil {
		return nil, errors.New("dispatchertls: the TLS config has no certificate")
	}
	if len(tlsConfig.NextProtos) == 0 {
		tlsConfig.NextProtos = []string{"http/1.1"}
	}
	return tlsConfig, nil
}

func allowedHosts(cfg Config) []string {
	seen := map[string]struct{}{}
	hosts := []string{}
	add := func(host string) {
		host = strings.ToLower(hostport.HostnameOnly(strings.TrimSpace(host)))
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	for _, host := range cfg.Hostnames {
		add(host)
	}
	for _, host := range cfg.AllowedHosts {
		add(host)
	}
	if len(hosts) == 0 {
		hosts = []string{"localhost"}
	}
	return hosts
}

func hostAllowed(hosts []string, host string) bool {
	host = strings.ToLower(hostport.HostnameOnly(host))
	for _, allowed := range hosts {
		if allowed == "*" || host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func probeHost(hosts []string) string {
	for _, host := range hosts {
		if host != "*" {
			return host
		}
	}
	return "localhost"
}

func probeClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: transport}
}

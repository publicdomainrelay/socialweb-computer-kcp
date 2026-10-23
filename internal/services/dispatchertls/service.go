package dispatchertls

import (
	"cmp"
	"context"
	"crypto/tls"
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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "DispatcherTLS"

const shutdownTimeout = 5 * time.Second

type Deps struct {
	component.Instance
	Registry *component.Registry
	TLS      *tls.Config
}

type Service struct {
	spec v1alpha1.DispatcherTLSSpec
	deps Deps

	workspace    string
	allowedHosts []string
	certFile     string
	caBundle     string
	upstreamURL  string

	mu       sync.Mutex
	log      *slog.Logger
	http     *http.Server
	listener net.Listener
	conns    *connSet
	stopCh   chan struct{}
	stopOnce sync.Once
	ready    bool
}

func New(spec v1alpha1.DispatcherTLSSpec, deps Deps) *Service {
	s := &Service{
		spec:      spec,
		deps:      deps,
		workspace: cmp.Or(strings.TrimSpace(deps.Self.Workspace), "root"),
	}
	hostnames := append([]string(nil), spec.Hostnames...)
	if spec.TLS != nil {
		hostnames = strs.AppendUniqueFold(hostnames, spec.TLS.Hostnames...)
		s.caBundle = strings.TrimSpace(spec.TLS.CABundle)
		if spec.TLS.CertRef != "" || spec.TLS.Enabled {
			s.certFile, _ = servetls.CertificateFiles(spec.TLS.CertRef, spec.Storage, hostnames)
		}
	}
	s.allowedHosts = allowedHosts(hostnames)
	if target, ok := upstreamEndpoint(deps.Registry, s.workspace, spec.UpstreamRef); ok {
		s.upstreamURL = target
	}
	return s
}

var _ component.Service = (*Service)(nil)

func (s *Service) listenAddress() string {
	if s.spec.Serve == nil {
		return hostport.Listen("", "", 0)
	}
	return hostport.Listen("", s.spec.Serve.Host, int(s.spec.Serve.Port))
}

func upstreamEndpoint(registry *component.Registry, workspace string, ref *commonv1.ObjectRef) (string, bool) {
	if ref == nil || registry == nil {
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

func (s *Service) Start(ctx context.Context) error {
	if s.upstreamURL == "" && s.spec.UpstreamRef == nil {
		return errors.New("dispatchertls: the upstream is required")
	}
	upstreamURL := s.upstreamURL
	if target, ok := s.resolveUpstream(upstreamURL); ok {
		upstreamURL = target
	}
	log := s.deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tlsConfig, err := s.listenerTLS()
	if err != nil {
		return err
	}
	hosts := s.allowedHosts
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if s.caBundle != "" {
		pool, err := tlsreload.PoolFromBundle(s.caBundle, log)
		if err != nil {
			return fmt.Errorf("dispatchertls: %w", err)
		}
		transport.DialTLSContext = pool.DialTLSContext(&tls.Config{MinVersion: tls.VersionTLS12})
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
		raw, ok := s.resolveUpstream(upstreamURL)
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
	listener, err := net.Listen("tcp", s.listenAddress())
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
	s.log = log
	s.http = httpServer
	s.listener = listener
	s.conns = conns
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("dispatcher_tls_listening", "address", listener.Addr().String(), "upstream", upstreamURL, "hostnames", hosts)

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
	hosts := s.allowedHosts
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
	s.mu.Unlock()
	if httpServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
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

func (s *Service) resolveUpstream(upstreamURL string) (string, bool) {
	if raw := strings.TrimSpace(upstreamURL); raw != "" {
		return raw, true
	}
	return upstreamEndpoint(s.deps.Registry, s.workspace, s.spec.UpstreamRef)
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

func (s *Service) listenerTLS() (*tls.Config, error) {
	if s.certFile != "" {
		return servetls.Config(s.certFile, nil, s.deps.Log)
	}
	tlsConfig := s.deps.TLS
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

func allowedHosts(hostnames []string) []string {
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
	for _, host := range hostnames {
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

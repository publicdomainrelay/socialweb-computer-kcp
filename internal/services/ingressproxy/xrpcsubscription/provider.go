package xrpcsubscription

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
)

const DefaultShutdownTimeout = 5 * time.Second

type Config struct {
	Address string

	Host string

	Port int

	Hostname string

	Hostnames []string

	ServiceID string

	AdditionalHosts []string

	AllowedDIDs []string

	AllowedHosts []string

	PLCURL string

	Directory identity.Directory

	CertFile string

	KeyFile string

	TLS *tls.Config

	RelayURL string

	RelayTimeout time.Duration

	KeepaliveInterval time.Duration

	KeepaliveTimeout time.Duration

	ReconnectGrace time.Duration

	NonceTTL time.Duration

	ShutdownTimeout time.Duration

	Identity exposure.IngressIdentity
}

type Provider struct {
	cfg Config

	mu sync.Mutex

	log *slog.Logger

	server *ingress.Server

	http *http.Server

	listener net.Listener

	stopCh chan struct{}

	stopOnce sync.Once

	ready bool

	registrar exposure.IngressRegistrar
}

func New(cfg Config) *Provider {
	return &Provider{cfg: cfg}
}

func (p *Provider) Start(ctx context.Context, deps component.Deps) error {
	cfg := p.cfg
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	hostname := primaryHostname(cfg.Hostname, cfg.Hostnames)
	server, err := ingress.NewServer(ingress.Config{
		Hostname:          hostname,
		ServiceID:         cfg.ServiceID,
		RelayTimeout:      cfg.RelayTimeout,
		KeepaliveInterval: cfg.KeepaliveInterval,
		KeepaliveTimeout:  cfg.KeepaliveTimeout,
		ReconnectGrace:    cfg.ReconnectGrace,
		NonceTTL:          cfg.NonceTTL,
		AdditionalHosts:   append([]string(nil), cfg.AdditionalHosts...),
		AllowedDIDs:       append([]string(nil), cfg.AllowedDIDs...),
		AllowedHosts:      append([]string(nil), cfg.AllowedHosts...),
		Directory:         cfg.Directory,
		PLCURL:            cfg.PLCURL,
		Logger:            log,
	})
	if err != nil {
		return fmt.Errorf("ingressproxy: building the server: %w", err)
	}
	rawListener, err := net.Listen("tcp", listenAddress(cfg.Address, cfg.Host, cfg.Port))
	if err != nil {
		server.Close()
		return fmt.Errorf("ingressproxy: listening: %w", err)
	}
	var listener net.Listener = rawListener
	if cfg.TLS != nil {
		listener = tls.NewListener(rawListener, cfg.TLS)
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	stopCh := make(chan struct{})
	registrar := exposure.NewSubscriberRegistrar(deps, cfg.Identity)

	p.mu.Lock()
	if p.http != nil {
		p.mu.Unlock()
		_ = listener.Close()
		server.Close()
		return errors.New("ingressproxy: the service already started")
	}
	p.cfg = cfg
	p.log = log
	p.server = server
	p.http = httpServer
	p.listener = listener
	p.stopCh = stopCh
	p.stopOnce = sync.Once{}
	p.ready = true
	p.registrar = registrar
	p.mu.Unlock()

	log.Info("ingressproxy_listening", "provider", "xrpc-subscription", "address", listener.Addr().String(),
		"hostname", hostname, "relay", cfg.RelayURL)

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(listener) }()

	select {
	case <-ctx.Done():
	case <-stopCh:
	case serveErr := <-errCh:
		server.Close()
		p.finish()
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("ingressproxy: serving: %w", serveErr)
		}
		return nil
	}

	p.shutdown(ctx)
	server.Close()
	serveErr := <-errCh
	p.finish()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("ingressproxy: serving: %w", serveErr)
	}
	return nil
}

func (p *Provider) Ready(ctx context.Context) error {
	p.mu.Lock()
	ready := p.ready
	address := ""
	secure := p.cfg.TLS != nil
	if p.listener != nil {
		address = p.listener.Addr().String()
	}
	hostname := primaryHostname(p.cfg.Hostname, p.cfg.Hostnames)
	p.mu.Unlock()
	if !ready || address == "" {
		return errors.New("ingressproxy: the service is not listening")
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+address+"/.well-known/did.json", nil)
	if err != nil {
		return err
	}
	req.Host = hostnameOnly(hostname)
	resp, err := probeClient(secure).Do(req)
	if err != nil {
		return fmt.Errorf("ingressproxy: health request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ingressproxy: health returned %d", resp.StatusCode)
	}
	return nil
}

func (p *Provider) Endpoints() []component.Endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener == nil {
		return nil
	}
	address := p.listener.Addr().String()
	hostname := primaryHostname(p.cfg.Hostname, p.cfg.Hostnames)
	if p.cfg.TLS != nil {
		return []component.Endpoint{
			{Name: "https", Protocol: component.ProtocolHTTPS, URL: "https://" + address, Address: address},
			{Name: "control", Protocol: component.ProtocolHTTPS, URL: "https://" + hostnameOnly(hostname), Address: address},
		}
	}
	return []component.Endpoint{
		{Name: "http", Protocol: component.ProtocolHTTP, URL: "http://" + address, Address: address},
		{Name: "control", Protocol: component.ProtocolHTTP, URL: "http://" + hostnameOnly(hostname), Address: address},
	}
}

func (p *Provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	stopCh := p.stopCh
	p.mu.Unlock()
	if stopCh == nil {
		return nil
	}
	p.stopOnce.Do(func() { close(stopCh) })
	p.shutdown(ctx)
	return nil
}

func (p *Provider) IngressRegistrar() exposure.IngressRegistrar {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.registrar
}

func (p *Provider) shutdown(ctx context.Context) {
	p.mu.Lock()
	httpServer := p.http
	timeout := p.cfg.ShutdownTimeout
	p.mu.Unlock()
	if httpServer == nil {
		return
	}
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		_ = httpServer.Close()
	}
}

func (p *Provider) finish() {
	p.mu.Lock()
	p.ready = false
	p.listener = nil
	p.http = nil
	p.mu.Unlock()
}

func listenAddress(address, host string, port int) string {
	if address != "" {
		return address
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
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

func probeClient(secure bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if secure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: transport}
}

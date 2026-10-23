package plc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atplc "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsreload"
)

const Kind = "Plc"

const DefaultHost = "127.0.0.1"

const DefaultShutdownTimeout = 5 * time.Second

type Config struct {
	Deps            component.Deps
	Key             component.Key
	StateDir        string
	Address         string
	Host            string
	Port            int
	Hostnames       []string
	Network         *v1alpha1.MarketNetworkValue
	CertFile        string
	KeyFile         string
	TLS             *tls.Config
	Version         string
	ShutdownTimeout time.Duration
}

type Service struct {
	cfg Config

	mu       sync.Mutex
	log      *slog.Logger
	store    atplc.Store
	server   *atplc.Server
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
	var decoded v1alpha1.PlcSpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("plc: decoding the component spec: %w", err)
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	cfg := Config{
		Deps:      deps,
		Key:       component.Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name},
		StateDir:  stateDir,
		Host:      DefaultHost,
		Hostnames: append([]string(nil), decoded.Hostnames...),
	}
	if decoded.Network != nil {
		network := *decoded.Network
		cfg.Network = &network
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
		cfg.Hostnames = appendHostnames(cfg.Hostnames, decoded.TLS.Hostnames...)
	}
	if decoded.Network != nil {
		cfg.Hostnames = appendHostnames(cfg.Hostnames, hostport.NetworkHostnames(decoded.Network, cfg.Port)...)
	}
	if decoded.TLS != nil && decoded.TLS.Enabled {
		certFile, keyFile := certificateFiles(decoded.TLS.CertRef, decoded.Storage, cfg.Hostnames)
		cfg.CertFile = certFile
		cfg.KeyFile = keyFile
		tlsConfig, err := serverTLS(deps, cfg.CertFile)
		if err != nil {
			return Config{}, fmt.Errorf("plc: %w", err)
		}
		cfg.TLS = tlsConfig
	}
	return cfg, nil
}

func appendHostnames(hostnames []string, extra ...string) []string {
	seen := make(map[string]struct{}, len(hostnames)+len(extra))
	out := make([]string, 0, len(hostnames)+len(extra))
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		key := strings.ToLower(host)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, host)
	}
	for _, host := range hostnames {
		add(host)
	}
	for _, host := range extra {
		add(host)
	}
	return out
}

func certificateFiles(certRef string, storage *v1alpha1.ComponentStorage, hostnames []string) (string, string) {
	certFile := refPath(certRef, storage)
	if certFile == "" && storage != nil && storage.CertsDir != "" && len(hostnames) > 0 {
		name := hostport.HostnameOnly(hostnames[0])
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
	if before, ok := strings.CutSuffix(certFile, ".pem"); ok {
		candidates = append(candidates, before+"-key.pem")
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

func Register(runtime *component.Runtime) error {
	return runtime.Register(Kind, Factory)
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	cfg := s.cfg
	cfg.Deps = mergeDeps(cfg.Deps, deps)
	log := cfg.Deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = cfg.Deps.StateDir
	}
	var store atplc.Store = atplc.NewMemoryStore()
	if stateDir != "" {
		fs, err := atplc.NewFileStore(filepath.Join(stateDir, "plc.json"))
		if err != nil {
			return fmt.Errorf("plc: opening the operation log: %w", err)
		}
		store = fs
	}
	server := atplc.NewServer(atplc.Options{Store: store, Version: cfg.Version})
	key := cfg.Key
	if key.Kind == "" {
		key.Kind = Kind
	}
	if key.Name == "" {
		key.Name = "default"
	}
	handlers := cfg.Deps.Handlers
	if handlers == nil {
		return errors.New("plc: no handler registry is available")
	}
	stopCh := make(chan struct{})
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return errors.New("plc: the service already started")
	}
	s.stopCh = stopCh
	s.mu.Unlock()
	host := logicalHost(key)
	baseURL := "http://" + host
	if err := handlers.RegisterHTTP(key, server.Handler()); err != nil {
		s.mu.Lock()
		s.stopCh = nil
		s.mu.Unlock()
		return fmt.Errorf("plc: registering the handler: %w", err)
	}

	s.mu.Lock()
	s.cfg = cfg
	s.log = log
	s.store = store
	s.server = server
	s.key = key
	s.baseURL = baseURL
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("plc_registered", "host", host, "hostname", hostport.Primary(cfg.Host, cfg.Hostnames))

	select {
	case <-ctx.Done():
	case <-stopCh:
	}
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
		return errors.New("plc: the service is not registered")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := handlers.HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("plc: health request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plc: health returned %d", resp.StatusCode)
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

func (s *Service) Store() atplc.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store
}

func (s *Service) Server() *atplc.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server
}

func (s *Service) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseURL == "" {
		return ""
	}
	return strings.TrimPrefix(s.baseURL, "http://")
}

func (s *Service) finish() {
	s.mu.Lock()
	s.ready = false
	s.stopCh = nil
	s.mu.Unlock()
}

func logicalHost(key component.Key) string {
	kind := strings.ToLower(strings.TrimSpace(key.Kind))
	if kind == "" {
		return ""
	}
	return kcp.ServiceFQDN(kind, "default", key.Workspace, kcp.DefaultServiceDomain)
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
	if out.Handlers == nil {
		out.Handlers = deps.Handlers
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

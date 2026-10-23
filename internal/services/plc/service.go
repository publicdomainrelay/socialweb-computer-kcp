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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
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
		cfg.Hostnames = strs.AppendUniqueFold(cfg.Hostnames, decoded.TLS.Hostnames...)
	}
	if decoded.Network != nil {
		cfg.Hostnames = strs.AppendUniqueFold(cfg.Hostnames, hostport.NetworkHostnames(decoded.Network, cfg.Port)...)
	}
	if decoded.TLS != nil && decoded.TLS.Enabled {
		certFile, keyFile := servetls.CertificateFiles(decoded.TLS.CertRef, decoded.Storage, cfg.Hostnames)
		cfg.CertFile = certFile
		cfg.KeyFile = keyFile
		tlsConfig, err := servetls.Config(cfg.CertFile, deps.TLS, deps.Log)
		if err != nil {
			return Config{}, fmt.Errorf("plc: %w", err)
		}
		cfg.TLS = tlsConfig
	}
	return cfg, nil
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
	cfg.Deps = cfg.Deps.Or(deps)
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
	host := kcp.KindServiceHost(key.Kind, key.Workspace)
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

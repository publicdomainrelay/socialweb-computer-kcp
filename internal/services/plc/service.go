package plc

import (
	"cmp"
	"context"
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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const Kind = "Plc"

const DefaultShutdownTimeout = 5 * time.Second

type Service struct {
	spec v1alpha1.PlcSpec
	in   component.Instance

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

func New(spec v1alpha1.PlcSpec, in component.Instance) *Service {
	return &Service{spec: spec, in: in}
}

func (s *Service) stateDir() string {
	if s.spec.Storage != nil && s.spec.Storage.DataDir != "" {
		return s.spec.Storage.DataDir
	}
	return s.in.StateDir
}

func (s *Service) hostnames() []string {
	out := append([]string(nil), s.spec.Hostnames...)
	if s.spec.TLS != nil {
		out = strs.AppendUniqueFold(out, s.spec.TLS.Hostnames...)
	}
	if s.spec.Serve != nil {
		out = strs.AppendUniqueFold(out, hostport.NetworkHostnames(s.spec.Network, int(s.spec.Serve.Port))...)
	}
	return out
}

func (s *Service) serveHost() string {
	if s.spec.Serve == nil {
		return ""
	}
	return s.spec.Serve.Host
}

func (s *Service) Start(ctx context.Context) error {
	log := s.in.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	path := ""
	if dir := s.stateDir(); dir != "" {
		path = filepath.Join(dir, "plc.json")
	}
	store, err := atplc.OpenStore(path)
	if err != nil {
		return fmt.Errorf("plc: opening the operation log: %w", err)
	}
	server := atplc.NewServer(atplc.Options{Store: store})
	key := s.in.Key()
	key.Kind = cmp.Or(key.Kind, Kind)
	key.Name = cmp.Or(key.Name, "default")
	handlers := s.in.Bus
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
	s.log = log
	s.store = store
	s.server = server
	s.key = key
	s.baseURL = baseURL
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.ready = true
	s.mu.Unlock()

	log.Info("plc_registered", "host", host, "hostname", hostport.Primary(s.serveHost(), s.hostnames()))

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
	handlers := s.in.Bus
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

var _ component.Service = (*Service)(nil)

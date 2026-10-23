package sshserver

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/ownersession"
)

type Service struct {
	spec v1alpha1.SshServerSpec

	deps Deps

	cfg gateway.Config

	log *slog.Logger

	ownerSessions map[string]ownersession.Session

	tls *tls.Config

	directory identity.Directory

	mu sync.Mutex

	started bool

	ready bool

	stopped bool

	ctx context.Context

	cancel context.CancelFunc

	done chan struct{}

	key component.Key

	httpURL string

	sshURL string

	gateway *gateway.Gateway
}

var (
	_ component.Service  = (*Service)(nil)
	_ sshcontract.Runner = requester{}
)

type requester struct {
	service *Service

	ref commonv1.ObjectRef
}

func (r requester) RunCommand(ctx context.Context, account sshcontract.Account, command string, env map[string]string, io sshcontract.CommandIO) error {
	runner, err := r.service.resolveRequester(ctx, r.ref)
	if err != nil {
		return err
	}
	return runner.RunCommand(ctx, account, command, env, io)
}

func (s *Service) resolveRequester(ctx context.Context, ref commonv1.ObjectRef) (sshcontract.Runner, error) {
	runner, err := bus.Resolve[sshcontract.Runner](s.deps.Caller(ctx), s.deps.Bus, s.deps.Self, ref,
		bus.Capability{Name: sshcontract.CapabilityRun, Scope: []string{sshcontract.ScopeRun}})
	if err != nil {
		return nil, fmt.Errorf("sshserver: the Requester %s/%s: %w", ref.Workspace, ref.Name, err)
	}
	return runner, nil
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("sshserver: the service is already started")
	}
	s.started = true
	s.ready = false
	s.done = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.ctx = runCtx
	s.cancel = cancel
	s.mu.Unlock()

	fail := func(err error) error {
		cancel()
		s.shutdown(ctx)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return err
	}
	if err := s.setup(runCtx); err != nil {
		return fail(err)
	}
	key := s.deps.Key()
	key.Kind = cmp.Or(key.Kind, Kind)
	key.Name = cmp.Or(key.Name, "default")
	key.Workspace = s.workspace()
	handlers := s.deps.Bus
	if handlers == nil {
		return fail(errors.New("sshserver: no handler registry is available"))
	}
	host := kcp.KindServiceHost(key.Kind, key.Workspace)
	httpURL := "http://" + host
	sshURL := "ssh://" + host
	if err := handlers.RegisterHTTP(key, s.handler()); err != nil {
		return fail(fmt.Errorf("sshserver: registering the handler: %w", err))
	}
	stream := func(streamCtx context.Context, conn net.Conn) error {
		return s.gateway.ServeConn(streamCtx, conn)
	}
	if err := handlers.RegisterStream(key, stream, "ssh"); err != nil {
		handlers.Remove(key)
		return fail(fmt.Errorf("sshserver: registering the stream: %w", err))
	}
	s.mu.Lock()
	s.key = key
	s.httpURL = httpURL
	s.sshURL = sshURL
	s.ready = true
	s.mu.Unlock()
	s.log.Info("sshserver_registered", "http", host, "ssh", host)

	<-runCtx.Done()
	s.shutdown(ctx)
	close(s.done)
	return nil
}

func (s *Service) setup(ctx context.Context) error {
	ref, err := s.requesterRef()
	if err != nil {
		return err
	}
	if _, err := s.resolveRequester(ctx, ref); err != nil {
		return err
	}
	owners := ownersession.New(ownersession.Config{Sessions: s.ownerSessions}, ownersession.Deps{Open: s.openOwner})
	g, err := gateway.New(s.cfg, gateway.Deps{
		Runner:    requester{service: s, ref: ref},
		Directory: s.directory,
		Trust:     s.inProcess,
		Client:    s.httpClient(requestTimeout),
		Auth:      owners,
		Agents:    owners,
		Log:       s.log,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.gateway = g
	s.mu.Unlock()
	return nil
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /connect.json", s.connect)
	mux.HandleFunc("GET /.well-known/cocore-app.json", s.cocoreApp)
	s.gateway.Register(mux)
	return mux
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) connect(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{"sshHost": s.cfg.Hostname})
}

func (s *Service) cocoreApp(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DID == "" {
		httpjson.Write(w, http.StatusNotFound, map[string]any{"error": "NotFound"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"did": s.cfg.DID})
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	ready := s.ready
	g := s.gateway
	key := s.key
	handlers := s.deps.Bus
	s.mu.Unlock()
	if !started {
		return errors.New("sshserver: the service is not started")
	}
	if !ready {
		return errors.New("sshserver: the service is starting")
	}
	if g == nil {
		return errors.New("sshserver: no Requester is configured")
	}
	if handlers == nil {
		return errors.New("sshserver: no handler registry is available")
	}
	if _, ok := handlers.Handler(key); !ok {
		return errors.New("sshserver: the HTTP handler is not registered")
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpURL == "" || s.sshURL == "" {
		return nil
	}
	return []component.Endpoint{
		{Name: "http", Protocol: component.ProtocolHTTP, URL: s.httpURL},
		{Name: "ssh", Protocol: component.ProtocolSSH, URL: s.sshURL},
	}
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) shutdown(ctx context.Context) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	g := s.gateway
	s.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultShutdownTimeout)
	defer cancel()
	if g != nil {
		if err := g.Shutdown(stopCtx); err != nil {
			s.log.Warn("sshserver_shutdown_failed", "err", err)
		}
	}
	s.log.Info("sshserver_stopped")
	s.mu.Lock()
	s.started = false
	s.ready = false
	s.mu.Unlock()
}

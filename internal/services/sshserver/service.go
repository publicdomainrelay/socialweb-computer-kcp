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
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/ownersession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

type Service struct {
	spec v1alpha1.SshServerSpec

	deps Deps

	cfg gateway.Config

	log *slog.Logger

	ownerSessions map[string]ownersession.Session

	tls *tls.Config

	trust *component.Trust

	directory identity.Directory

	mu sync.Mutex

	started bool

	ready bool

	readiness component.ReadySignal

	stopped bool

	ctx context.Context

	cancel context.CancelFunc

	done chan struct{}

	key component.Key

	httpURL string

	sshURL string

	publicURL string

	public net.Listener

	webURL string

	webServer *http.Server

	web *webSignIn

	perUser bool

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
	s := r.service
	if !s.perUser {
		runner, err := s.resolveRequester(ctx, r.ref)
		if err != nil {
			return err
		}
		return runner.RunCommand(ctx, account, command, env, io)
	}
	runner, err := s.resolveRequester(ctx, s.accountRequesterRef(account.DID))
	switch {
	case errors.Is(err, bus.ErrLinkDenied):
		s.log.Info("sshserver_account_not_signed_in", "did", account.DID)
		_ = io.WriteErr([]byte("sign in at " + s.publicOrigin() + " first: this ssh gateway rents VMs as you, with your own atproto session, and has none for " + account.DID + " yet\n"))
		io.Exit(1)
		return nil
	case err != nil:
		s.log.Warn("sshserver_account_requester_unavailable", "did", account.DID, "err", err.Error())
		_ = io.WriteErr([]byte("ssh gateway error: your requester is not available: " + err.Error() + "\n"))
		io.Exit(1)
		return nil
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
	stopTrust, err := s.trust.Follow(runCtx, s.deps.Instance)
	if err != nil {
		return fail(err)
	}
	defer stopTrust()
	if err := s.setup(runCtx); err != nil {
		return fail(err)
	}
	defer s.followVault(runCtx)()
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
	publicURL := ""
	if addr := strings.TrimSpace(s.spec.PublicListen); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			handlers.Remove(key)
			return fail(fmt.Errorf("sshserver: the public ssh listener %s: %w", addr, err))
		}
		publicURL = "ssh://" + ln.Addr().String()
		s.mu.Lock()
		s.public = ln
		s.mu.Unlock()
		go s.servePublic(runCtx, ln)
	}
	webURL := ""
	if addr := strings.TrimSpace(s.spec.WebListen); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			handlers.Remove(key)
			return fail(fmt.Errorf("sshserver: the web listener %s: %w", addr, err))
		}
		webURL = "http://" + ln.Addr().String()
		srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: webReadHeaderTimeout}
		s.mu.Lock()
		s.webServer = srv
		s.mu.Unlock()
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Warn("sshserver_web_serve_failed", "err", err)
			}
		}()
	}
	s.mu.Lock()
	s.key = key
	s.httpURL = httpURL
	s.sshURL = sshURL
	s.publicURL = publicURL
	s.webURL = webURL
	s.ready = true
	s.mu.Unlock()
	s.log.Info("sshserver_registered", "http", host, "ssh", host, "public", publicURL, "web", webURL)
	s.readiness.Signal()

	<-runCtx.Done()
	s.shutdown(ctx)
	close(s.done)
	return nil
}

func (s *Service) servePublic(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("sshserver_public_accept_failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(publicAcceptBackoff):
			}
			continue
		}
		go func() {
			defer conn.Close()
			if err := s.gateway.ServeConn(ctx, conn); err != nil && ctx.Err() == nil {
				s.log.Info("sshserver_public_conn_closed", "err", err)
			}
		}()
	}
}

func (s *Service) setup(ctx context.Context) error {
	var ref commonv1.ObjectRef
	if !s.perUser {
		var err error
		if ref, err = s.requesterRef(); err != nil {
			return err
		}
	}
	web, err := s.openSignIn(ctx)
	if err != nil {
		return err
	}
	if !s.perUser {
		if _, err := s.resolveRequester(ctx, ref); err != nil {
			return err
		}
	}
	owners := ownersession.New(ownersession.Config{Sessions: s.ownerSessions}, ownersession.Deps{Open: s.openOwner})
	var auth sshtransport.Authenticator = owners
	var agents sshtransport.AgentProvider = owners
	if web != nil {
		auth, agents = liveAuth{s}, liveAgents{s}
	}
	g, err := gateway.New(s.cfg, gateway.Deps{
		Runner:    requester{service: s, ref: ref},
		Directory: s.directory,
		Trust:     s.inProcess,
		Client:    s.httpClient(requestTimeout),
		Auth:      auth,
		Agents:    agents,
		Log:       s.log,
	})
	if err != nil {
		web.close()
		return err
	}
	s.mu.Lock()
	s.gateway = g
	s.web = web
	s.mu.Unlock()
	return nil
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /connect.json", s.connect)
	mux.HandleFunc("GET /.well-known/cocore-app.json", s.cocoreApp)
	if s.currentWeb() != nil {
		for _, route := range oauthRoutes {
			mux.HandleFunc(route, s.oauthRoute)
		}
		mux.HandleFunc("GET /{$}", s.page)
	}
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

func (s *Service) Ready() component.Readiness {
	return &s.readiness
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpURL == "" || s.sshURL == "" {
		return nil
	}
	out := []component.Endpoint{
		{Name: "http", Protocol: component.ProtocolHTTP, URL: s.httpURL},
		{Name: "ssh", Protocol: component.ProtocolSSH, URL: s.sshURL},
	}
	if s.publicURL != "" {
		out = append(out, component.Endpoint{Name: PublicEndpoint, Protocol: component.ProtocolSSH, URL: s.publicURL})
	}
	if s.webURL != "" {
		out = append(out, component.Endpoint{Name: WebEndpoint, Protocol: component.ProtocolHTTP, URL: s.webURL})
	}
	return out
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
	public := s.public
	s.public = nil
	web := s.web
	s.web = nil
	webServer := s.webServer
	s.webServer = nil
	s.mu.Unlock()
	defer web.close()
	if webServer != nil {
		_ = webServer.Close()
	}
	if public != nil {
		_ = public.Close()
	}
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
	s.publicURL = ""
	s.webURL = ""
	s.mu.Unlock()
}

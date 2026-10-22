package sshserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	coreapi "github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

type Service struct {
	cfg Config

	log *slog.Logger

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

	sshServer *sshtransport.Server

	requester CommandRunner
}

type runnerAdapter struct {
	runner CommandRunner
}

func (a runnerAdapter) Run(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error {
	return a.runner.RunCommand(ctx, account, command, env, io)
}

type denyAuth struct{}

func (denyAuth) Authenticate(r *http.Request) (*coreapi.WebCaller, error) {
	return nil, nil
}

type signInAgents struct {
	signIn coreapi.WebSignIn
}

func (a signInAgents) AgentFor(caller *coreapi.WebCaller) coreapi.WebAccountAgent {
	if a.signIn == nil {
		return unavailableAgent{}
	}
	return a.signIn.AgentFor(caller)
}

type unavailableAgent struct{}

func (unavailableAgent) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return "", errors.New("sshserver: no account session is configured")
}

func (unavailableAgent) ListRecords(ctx context.Context, did, collection string, limit int) ([]coreapi.Record, error) {
	return nil, errors.New("sshserver: no account session is configured")
}

func (unavailableAgent) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (coreapi.RecordRef, error) {
	return coreapi.RecordRef{}, errors.New("sshserver: no account session is configured")
}

func (unavailableAgent) ApplyWrites(ctx context.Context, did string, writes []coreapi.Write) error {
	return errors.New("sshserver: no account session is configured")
}

type unavailableAgents struct{}

func (unavailableAgents) AgentFor(caller *coreapi.WebCaller) coreapi.WebAccountAgent {
	return unavailableAgent{}
}

type memoryCocoreTokens struct {
	mu sync.Mutex

	tokens map[string]sshtransport.CocoreToken
}

func (m *memoryCocoreTokens) Get(ctx context.Context, did string) (*sshtransport.CocoreToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.tokens[did]
	if !ok {
		return nil, nil
	}
	return &token, nil
}

func (m *memoryCocoreTokens) Set(ctx context.Context, did string, token *sshtransport.CocoreToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens == nil {
		m.tokens = map[string]sshtransport.CocoreToken{}
	}
	m.tokens[did] = *token
	return nil
}

func New(cfg Config) (*Service, error) {
	cfg.defaults()
	if err := cfg.check(); err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg, log: log.With("component", "sshserver", "did", cfg.DID)}, nil
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("sshserver: the service is already started")
	}
	s.cfg = s.cfg.withDeps(deps)
	if s.cfg.Log != nil {
		s.log = s.cfg.Log.With("component", "sshserver", "did", s.cfg.DID)
	}
	s.started = true
	s.ready = false
	s.done = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.ctx = runCtx
	s.cancel = cancel
	s.mu.Unlock()

	if err := s.setup(runCtx); err != nil {
		cancel()
		s.shutdown(context.Background())
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return err
	}
	key := s.cfg.Key
	if key.Kind == "" {
		key.Kind = Kind
	}
	if key.Name == "" {
		key.Name = s.cfg.ComponentName
	}
	if key.Name == "" {
		key.Name = "default"
	}
	if key.Workspace == "" {
		key.Workspace = s.cfg.Workspace
	}
	handlers := s.cfg.Deps.Handlers
	if handlers == nil {
		s.shutdown(context.Background())
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return errors.New("sshserver: no handler registry is available")
	}
	host := logicalHost(key)
	httpURL := "http://" + host
	sshURL := "ssh://" + host
	if err := handlers.RegisterHTTP(key, s.handler()); err != nil {
		s.shutdown(context.Background())
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return fmt.Errorf("sshserver: registering the handler: %w", err)
	}
	stream := func(streamCtx context.Context, conn net.Conn) error {
		return s.sshServer.ServeConn(streamCtx, conn)
	}
	if err := handlers.RegisterStream(key, stream, "ssh"); err != nil {
		handlers.Remove(key)
		s.shutdown(context.Background())
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return fmt.Errorf("sshserver: registering the stream: %w", err)
	}
	s.mu.Lock()
	s.key = key
	s.httpURL = httpURL
	s.sshURL = sshURL
	s.ready = true
	s.mu.Unlock()
	s.log.Info("sshserver_registered", "http", host, "ssh", host)

	<-runCtx.Done()
	s.shutdown(context.Background())
	close(s.done)
	return nil
}

func (s *Service) setup(ctx context.Context) error {
	if s.cfg.StateDir != "" {
		if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
			return fmt.Errorf("sshserver: state dir: %w", err)
		}
	}
	if dir := filepath.Dir(s.cfg.HostKeyPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sshserver: host key dir: %w", err)
		}
	}
	runner, err := s.resolveRequester(ctx)
	if err != nil {
		return err
	}
	s.requester = runner
	authorizer, err := s.authorizer()
	if err != nil {
		return err
	}
	sshServer, err := sshtransport.NewServer(sshtransport.ServerOptions{
		Config: sshtransport.Config{
			Port:               s.cfg.SSHPort,
			Hostname:           s.cfg.SSHHost,
			HostKeyPath:        s.cfg.HostKeyPath,
			MaxConnections:     s.cfg.MaxConnections,
			MaxSessions:        s.cfg.MaxSessions,
			SessionsPerAccount: s.cfg.SessionsPerAccount,
			MaxAuthAttempts:    s.cfg.MaxAuthAttempts,
			AuthTimeout:        s.cfg.AuthTimeout,
			SessionMax:         s.cfg.SessionMax,
		},
		Authorizer:     authorizer,
		Runner:         runnerAdapter{runner: runner},
		DefaultCommand: s.cfg.DefaultCommand,
		Log:            s.log,
	})
	if err != nil {
		return err
	}
	s.sshServer = sshServer
	return nil
}

func (s *Service) resolveRequester(ctx context.Context) (CommandRunner, error) {
	if s.cfg.Requester != nil {
		return s.cfg.Requester, nil
	}
	if s.cfg.RequesterRef == nil || s.cfg.RequesterRef.Name == "" {
		return nil, errors.New("sshserver: no Requester is configured")
	}
	if s.cfg.Deps.Registry == nil {
		return nil, errors.New("sshserver: no component registry is available")
	}
	workspace := s.cfg.Workspace
	kind := s.cfg.RequesterRef.Kind
	if kind == "" {
		kind = "Requester"
	}
	service, ok := s.cfg.Deps.Registry.Service(workspace, kind, s.cfg.RequesterRef.Name)
	if !ok {
		return nil, fmt.Errorf("sshserver: the Requester %s/%s is not registered", workspace, s.cfg.RequesterRef.Name)
	}
	if runner, ok := service.(CommandRunner); ok {
		return runner, nil
	}
	if runner, ok := service.(sshtransport.ComputeCommandRunner); ok {
		return legacyCommandRunner{runner: runner}, nil
	}
	return nil, fmt.Errorf("sshserver: the Requester %s/%s does not run commands", workspace, s.cfg.RequesterRef.Name)
}

func (s *Service) authorizer() (sshtransport.KeyAuthorizer, error) {
	if s.cfg.KeyAuthorizer != nil {
		return s.cfg.KeyAuthorizer, nil
	}
	directory := s.cfg.Directory
	if directory == nil {
		directory = identity.DefaultDirectory()
	}
	resolver := s.cfg.IdentityResolver
	if resolver == nil {
		resolver = sshtransport.DirectoryResolver{Directory: directory}
	}
	records := s.cfg.RecordSource
	if records == nil {
		trusted := append([]string(nil), s.cfg.TrustedHosts...)
		trusted = append(trusted, internalHosts(s.cfg.Deps)...)
		records = &sshtransport.HTTPRecordSource{
			TrustedHosts: trusted,
			Client:       serviceHTTPClient(s.cfg.Deps, 30*time.Second),
		}
	}
	return sshtransport.NewPDSKeyAuthorizer(sshtransport.KeyAuthorizerOptions{
		Resolver:         resolver,
		Records:          records,
		CacheTTL:         s.cfg.AssociationTTL,
		NegativeCacheTTL: s.cfg.NegativeCacheTTL,
		MissRecheck:      s.cfg.MissRecheck,
		MaxCacheEntries:  s.cfg.CacheEntries,
		Log:              s.log,
	})
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /connect.json", s.connect)
	mux.HandleFunc("GET /.well-known/cocore-app.json", s.cocoreApp)
	if s.cfg.SignIn != nil {
		mux.HandleFunc("GET /oauth-client-metadata.json", s.clientMetadata)
	}
	auth := s.authenticator()
	agents := s.agents()
	keys, err := sshtransport.NewKeys(sshtransport.KeysOptions{Agents: agents, Auth: auth, Log: s.log})
	if err == nil {
		keys.Register(mux)
	}
	pairing, err := s.cocorePairing()
	if err == nil {
		cocore, apiErr := sshtransport.NewCocoreAPI(sshtransport.CocoreOptions{Pairing: pairing, Auth: auth, Log: s.log})
		if apiErr == nil {
			cocore.Register(mux)
		}
	}
	return mux
}

func (s *Service) authenticator() sshtransport.Authenticator {
	if s.cfg.Auth != nil {
		return s.cfg.Auth
	}
	if s.cfg.SignIn == nil {
		return denyAuth{}
	}
	auth, err := coreapi.NewRequestAuth(coreapi.RequestAuthOptions{
		SignIn:       s.cfg.SignIn,
		PublicOrigin: s.cfg.PublicOrigin,
		CookieName:   s.cfg.SessionCookie,
		Log:          s.log,
	})
	if err != nil {
		s.log.Warn("sshserver_auth_failed", "err", err)
		return denyAuth{}
	}
	return auth
}

func (s *Service) agents() sshtransport.AgentProvider {
	if s.cfg.Agents != nil {
		return s.cfg.Agents
	}
	if s.cfg.SignIn != nil {
		return signInAgents{signIn: s.cfg.SignIn}
	}
	return unavailableAgents{}
}

func (s *Service) cocorePairing() (*sshtransport.CocorePairing, error) {
	tokens := s.cfg.CocoreTokens
	if tokens == nil {
		if s.cfg.StateDir == "" {
			tokens = &memoryCocoreTokens{}
		} else {
			tokens = sshtransport.NewFileCocoreTokenStore(filepath.Join(s.cfg.StateDir, "cocore-tokens.json"), s.log)
		}
	}
	return sshtransport.NewCocorePairing(sshtransport.CocorePairingOptions{
		APIBaseURL: s.cfg.CocoreAPIBaseURL,
		Tokens:     tokens,
		AppName:    s.cfg.CocoreAppName,
		AppDID:     s.cfg.CocoreAppDID,
		KeyName:    s.cfg.CocoreKeyName,
		ReturnURL:  s.cfg.CocoreReturnURL,
		TTL:        s.cfg.CocoreTTL,
		Client:     serviceHTTPClient(s.cfg.Deps, 30*time.Second),
		Log:        s.log,
	})
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) connect(w http.ResponseWriter, r *http.Request) {
	host := s.cfg.SSHPublicHost
	if host == "" {
		host = s.cfg.PublicHostname
	}
	if host == "" {
		host = s.cfg.SSHHost
	}
	port := s.cfg.SSHPort
	if _, _, err := net.SplitHostPort(host); err != nil && port > 0 {
		host = net.JoinHostPort(host, fmt.Sprintf("%d", port))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sshHost": host})
}

func (s *Service) cocoreApp(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CocoreAppDID == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "NotFound"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"did": s.cfg.CocoreAppDID})
}

func (s *Service) clientMetadata(w http.ResponseWriter, r *http.Request) {
	raw, err := s.cfg.SignIn.ClientMetadata(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the vault is not answering"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	ready := s.ready
	requester := s.requester
	key := s.key
	handlers := s.cfg.Deps.Handlers
	s.mu.Unlock()
	if !started {
		return errors.New("sshserver: the service is not started")
	}
	if !ready {
		return errors.New("sshserver: the service is starting")
	}
	if requester == nil {
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
	if ctx == nil {
		ctx = context.Background()
	}
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
	sshServer := s.sshServer
	s.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()
	if sshServer != nil {
		if err := sshServer.Shutdown(stopCtx); err != nil {
			s.log.Warn("sshserver_shutdown_failed", "err", err)
		}
	}
	s.log.Info("sshserver_stopped")
	s.mu.Lock()
	s.started = false
	s.ready = false
	s.mu.Unlock()
}

func logicalHost(key component.Key) string {
	kind := strings.ToLower(strings.TrimSpace(key.Kind))
	if kind == "" {
		return ""
	}
	return kcp.ServiceFQDN(kind, "default", key.Workspace, kcp.DefaultServiceDomain)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

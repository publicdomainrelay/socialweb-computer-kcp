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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

type Service struct {
	spec v1alpha1.SshServerSpec

	deps Deps

	log *slog.Logger

	stateDir string

	hostKeyPath string

	did string

	ownerSessions map[string]OwnerSession

	tls *tls.Config

	directory identity.Directory

	trustedHosts []string

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

func (denyAuth) Authenticate(r *http.Request) (*gatewayhttp.WebCaller, error) {
	return nil, nil
}

type unavailableAgent struct{}

func (unavailableAgent) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return "", errors.New("sshserver: no account session is configured")
}

func (unavailableAgent) ListRecords(ctx context.Context, did, collection string, limit int) ([]gatewayhttp.Record, error) {
	return nil, errors.New("sshserver: no account session is configured")
}

func (unavailableAgent) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (gatewayhttp.RecordRef, error) {
	return gatewayhttp.RecordRef{}, errors.New("sshserver: no account session is configured")
}

func (unavailableAgent) ApplyWrites(ctx context.Context, did string, writes []gatewayhttp.Write) error {
	return errors.New("sshserver: no account session is configured")
}

type unavailableAgents struct{}

func (unavailableAgents) AgentFor(caller *gatewayhttp.WebCaller) gatewayhttp.WebAccountAgent {
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

	if err := s.setup(runCtx); err != nil {
		cancel()
		s.shutdown(ctx)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return err
	}
	key := s.deps.Key()
	key.Kind = cmp.Or(key.Kind, Kind)
	key.Name = cmp.Or(key.Name, "default")
	key.Workspace = s.workspace()
	handlers := s.deps.Bus
	if handlers == nil {
		s.shutdown(ctx)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return errors.New("sshserver: no handler registry is available")
	}
	host := kcp.KindServiceHost(key.Kind, key.Workspace)
	httpURL := "http://" + host
	sshURL := "ssh://" + host
	if err := handlers.RegisterHTTP(key, s.handler()); err != nil {
		s.shutdown(ctx)
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
		s.shutdown(ctx)
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
	s.shutdown(ctx)
	close(s.done)
	return nil
}

func (s *Service) setup(ctx context.Context) error {
	if s.stateDir != "" {
		if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
			return fmt.Errorf("sshserver: state dir: %w", err)
		}
	}
	if dir := filepath.Dir(s.hostKeyPath); dir != "" && dir != "." {
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
			Hostname:           DefaultSSHHost,
			HostKeyPath:        s.hostKeyPath,
			MaxConnections:     int(s.spec.MaxConnections),
			MaxSessions:        int(s.spec.MaxSessions),
			SessionsPerAccount: int(s.spec.SessionsPerAccount),
			MaxAuthAttempts:    int(s.spec.MaxAuthAttempts),
			AuthTimeout:        time.Duration(ptr.Deref(s.spec.AuthTimeoutSeconds, 0)) * time.Second,
			SessionMax:         time.Duration(ptr.Deref(s.spec.SessionMaxSeconds, 0)) * time.Second,
		},
		Authorizer:     authorizer,
		Runner:         runnerAdapter{runner: runner},
		DefaultCommand: DefaultDefaultCommand,
		Log:            s.log,
	})
	if err != nil {
		return err
	}
	s.sshServer = sshServer
	return nil
}

func (s *Service) resolveRequester(ctx context.Context) (CommandRunner, error) {
	ref := s.spec.RequesterRef
	if ref == nil || ref.Name == "" {
		return nil, errors.New("sshserver: no Requester is configured")
	}
	if s.deps.Registry == nil {
		return nil, errors.New("sshserver: no component registry is available")
	}
	workspace := s.workspace()
	kind := cmp.Or(ref.Kind, "Requester")
	service, ok := s.deps.Registry.Service(workspace, kind, ref.Name)
	if !ok {
		return nil, fmt.Errorf("sshserver: the Requester %s/%s is not registered", workspace, ref.Name)
	}
	if runner, ok := service.(CommandRunner); ok {
		return runner, nil
	}
	if runner, ok := service.(sshtransport.ComputeCommandRunner); ok {
		return legacyCommandRunner{runner: runner}, nil
	}
	return nil, fmt.Errorf("sshserver: the Requester %s/%s does not run commands", workspace, ref.Name)
}

func (s *Service) authorizer() (sshtransport.KeyAuthorizer, error) {
	directory := s.directory
	if directory == nil {
		directory = identity.DefaultDirectory()
	}
	trusted := append([]string(nil), s.trustedHosts...)
	trusted = append(trusted, internalHosts(s.deps.Registry)...)
	return sshtransport.NewPDSKeyAuthorizer(sshtransport.KeyAuthorizerOptions{
		Resolver: sshtransport.DirectoryResolver{Directory: directory},
		Records: &sshtransport.HTTPRecordSource{
			TrustedHosts: trusted,
			Client:       s.httpClient(30 * time.Second),
		},
		MaxCacheEntries: int(s.spec.CacheEntries),
		Log:             s.log,
	})
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /connect.json", s.connect)
	mux.HandleFunc("GET /.well-known/cocore-app.json", s.cocoreApp)
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
	var auths multiAuth
	if sessions := ownerSessionsFrom(s.ownerSessions); len(sessions) > 0 {
		auths = append(auths, ownerSessionAuth{sessions: sessions})
	}
	if len(auths) == 0 {
		return denyAuth{}
	}
	return auths
}

func (s *Service) agents() sshtransport.AgentProvider {
	var providers combinedAgents
	if sessions := ownerSessionsFrom(s.ownerSessions); len(sessions) > 0 {
		providers = append(providers, &ownerSessionAgents{
			sessions: sessions,
			client:   s.httpClient(ownerSessionRequestTimeout),
			log:      s.log,
		})
	}
	if len(providers) == 0 {
		return unavailableAgents{}
	}
	return providers
}

func (s *Service) cocorePairing() (*sshtransport.CocorePairing, error) {
	var tokens sshtransport.CocoreTokenStore = &memoryCocoreTokens{}
	if s.stateDir != "" {
		tokens = sshtransport.NewFileCocoreTokenStore(filepath.Join(s.stateDir, "cocore-tokens.json"), s.log)
	}
	return sshtransport.NewCocorePairing(sshtransport.CocorePairingOptions{
		APIBaseURL: DefaultCocoreAPIBaseURL,
		Tokens:     tokens,
		AppName:    DefaultCocoreAppName,
		AppDID:     s.did,
		KeyName:    DefaultCocoreAppName,
		TTL:        DefaultCocoreTTL,
		Client:     s.httpClient(30 * time.Second),
		Log:        s.log,
	})
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
	httpjson.Write(w, http.StatusOK, map[string]any{"sshHost": DefaultSSHHost})
}

func (s *Service) cocoreApp(w http.ResponseWriter, r *http.Request) {
	if s.did == "" {
		httpjson.Write(w, http.StatusNotFound, map[string]any{"error": "NotFound"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"did": s.did})
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	ready := s.ready
	requester := s.requester
	key := s.key
	handlers := s.deps.Bus
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
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultShutdownTimeout)
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

var _ component.Service = (*Service)(nil)

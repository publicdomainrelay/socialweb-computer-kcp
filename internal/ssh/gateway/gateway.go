package gateway

import (
	"cmp"
	"context"
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

	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const (
	DefaultHostname = "127.0.0.1"

	DefaultCommand = "bash"

	DefaultCocoreAPIBaseURL = "https://cocore.dev/api"

	DefaultCocoreAppName = "socialweb-computer-ssh"

	DefaultCocoreTTL = sshtransport.DefaultCocorePairingTTL

	CocoreTokensFile = "cocore-tokens.json"
)

type Config struct {
	Hostname string

	HostKeyPath string

	StateDir string

	DID string

	DefaultCommand string

	MaxConnections int

	MaxSessions int

	SessionsPerAccount int

	MaxAuthAttempts int

	AuthTimeout time.Duration

	SessionMax time.Duration

	CacheEntries int

	TrustedHosts []string

	CocoreAPIBaseURL string

	CocoreAppName string

	CocoreTTL time.Duration
}

type Deps struct {
	Runner sshcontract.Runner

	Directory identity.Directory

	Trust func(host string) bool

	Client *http.Client

	Auth sshtransport.Authenticator

	Agents sshtransport.AgentProvider

	Log *slog.Logger
}

type Gateway struct {
	cfg Config

	log *slog.Logger

	server *sshtransport.Server

	keys *sshtransport.Keys

	cocore *sshtransport.Cocore
}

var (
	_ sshcontract.Server                = (*Gateway)(nil)
	_ sshtransport.ComputeCommandRunner = runner{}
	_ sshtransport.CocoreTokenStore     = (*memoryTokens)(nil)
)

func New(cfg Config, deps Deps) (*Gateway, error) {
	if deps.Runner == nil {
		return nil, errors.New("sshserver: no Requester is configured")
	}
	if cfg.HostKeyPath == "" {
		return nil, errors.New("sshserver: the component names no host key path")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.StateDir != "" {
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return nil, fmt.Errorf("sshserver: state dir: %w", err)
		}
	}
	if dir := filepath.Dir(cfg.HostKeyPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sshserver: host key dir: %w", err)
		}
	}
	directory := deps.Directory
	if directory == nil {
		directory = identity.DefaultDirectory()
	}
	authorizer, err := sshtransport.NewPDSKeyAuthorizer(sshtransport.KeyAuthorizerOptions{
		Resolver: sshtransport.DirectoryResolver{Directory: directory},
		Records: &sshtransport.HTTPRecordSource{
			TrustedHosts: append([]string(nil), cfg.TrustedHosts...),
			Trust:        deps.Trust,
			Client:       deps.Client,
		},
		MaxCacheEntries: cfg.CacheEntries,
		Log:             log,
	})
	if err != nil {
		return nil, err
	}
	server, err := sshtransport.NewServer(sshtransport.ServerOptions{
		Config: sshtransport.Config{
			Hostname:           cmp.Or(cfg.Hostname, DefaultHostname),
			HostKeyPath:        cfg.HostKeyPath,
			MaxConnections:     cfg.MaxConnections,
			MaxSessions:        cfg.MaxSessions,
			SessionsPerAccount: cfg.SessionsPerAccount,
			MaxAuthAttempts:    cfg.MaxAuthAttempts,
			AuthTimeout:        cfg.AuthTimeout,
			SessionMax:         cfg.SessionMax,
		},
		Authorizer:     authorizer,
		Runner:         runner{runner: deps.Runner},
		DefaultCommand: cmp.Or(cfg.DefaultCommand, DefaultCommand),
		Log:            log,
	})
	if err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, log: log, server: server}
	if deps.Auth != nil && deps.Agents != nil {
		if keys, err := sshtransport.NewKeys(sshtransport.KeysOptions{Agents: deps.Agents, Auth: deps.Auth, Log: log}); err == nil {
			g.keys = keys
		}
		if pairing, err := g.pairing(deps.Client); err == nil {
			if cocore, err := sshtransport.NewCocoreAPI(sshtransport.CocoreOptions{Pairing: pairing, Auth: deps.Auth, Log: log}); err == nil {
				g.cocore = cocore
			}
		}
	}
	return g, nil
}

func (g *Gateway) pairing(client *http.Client) (*sshtransport.CocorePairing, error) {
	var tokens sshtransport.CocoreTokenStore = &memoryTokens{}
	if g.cfg.StateDir != "" {
		tokens = sshtransport.NewFileCocoreTokenStore(filepath.Join(g.cfg.StateDir, CocoreTokensFile), g.log)
	}
	appName := cmp.Or(g.cfg.CocoreAppName, DefaultCocoreAppName)
	ttl := g.cfg.CocoreTTL
	if ttl <= 0 {
		ttl = DefaultCocoreTTL
	}
	return sshtransport.NewCocorePairing(sshtransport.CocorePairingOptions{
		APIBaseURL: cmp.Or(g.cfg.CocoreAPIBaseURL, DefaultCocoreAPIBaseURL),
		Tokens:     tokens,
		AppName:    appName,
		AppDID:     g.cfg.DID,
		KeyName:    appName,
		TTL:        ttl,
		Client:     client,
		Log:        g.log,
	})
}

func (g *Gateway) Hostname() string {
	return cmp.Or(g.cfg.Hostname, DefaultHostname)
}

func (g *Gateway) DID() string {
	return g.cfg.DID
}

func (g *Gateway) Register(mux *http.ServeMux) {
	if g.keys != nil {
		g.keys.Register(mux)
	}
	if g.cocore != nil {
		g.cocore.Register(mux)
	}
}

func (g *Gateway) ServeConn(ctx context.Context, conn net.Conn) error {
	return g.server.ServeConn(ctx, conn)
}

func (g *Gateway) Shutdown(ctx context.Context) error {
	return g.server.Shutdown(ctx)
}

type runner struct {
	runner sshcontract.Runner
}

func (r runner) Run(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error {
	return r.runner.RunCommand(ctx, account, command, env, io)
}

type memoryTokens struct {
	mu sync.Mutex

	tokens map[string]sshtransport.CocoreToken
}

func (m *memoryTokens) Get(ctx context.Context, did string) (*sshtransport.CocoreToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.tokens[did]
	if !ok {
		return nil, nil
	}
	return &token, nil
}

func (m *memoryTokens) Set(ctx context.Context, did string, token *sshtransport.CocoreToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens == nil {
		m.tokens = map[string]sshtransport.CocoreToken{}
	}
	m.tokens[did] = *token
	return nil
}

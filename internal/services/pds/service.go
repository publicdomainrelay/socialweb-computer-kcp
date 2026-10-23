package pds

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/bluesky-social/indigo/util/ssrf"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

const (
	shutdownTimeout  = 10 * time.Second
	healthTimeout    = 2 * time.Second
	directoryTimeout = 10 * time.Second
)

type Service struct {
	cfg Config

	mu       sync.Mutex
	started  bool
	endpoint component.Endpoint
	key      component.Key
	baseURL  string
	server   *atpds.Server
	stopCh   chan struct{}
	stopOnce sync.Once

	closeOnce    sync.Once
	repoStore    *repo.SQLiteStore
	accountStore *atpds.Store
	keyStore     atpds.RepoKeyStore
}

var _ component.Service = (*Service)(nil)

func New(cfg Config) (*Service, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = atpds.ModeFull
	}
	if mode != atpds.ModeFull && mode != atpds.ModeRepo {
		return nil, fmt.Errorf("pds: unknown mode %q", mode)
	}
	if mode == atpds.ModeRepo && cfg.OAuth != nil {
		return nil, errors.New("pds: oauth is not available in read-only repo mode")
	}
	cfg.Mode = mode
	cfg.StateDir = strings.TrimSpace(cfg.StateDir)
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if cfg.RequireIngress && cfg.PublicBaseURL != "" {
		if err := publicEndpoint(cfg.PublicBaseURL); err != nil {
			return nil, err
		}
	}
	return &Service{cfg: cfg}, nil
}

func publicEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("pds: the ingress public URL %q is not a scheme, host and optional port", raw)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("pds: the ingress public URL %q must be https", raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("pds: the ingress public URL %q must carry no userinfo, path, query or fragment", raw)
	}
	return nil
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("pds: the service is already started")
	}
	s.mu.Unlock()

	cfg := s.cfg
	if cfg.Deps.Handlers == nil {
		cfg.Deps.Handlers = deps.Handlers
	}
	if cfg.Deps.Registry == nil {
		cfg.Deps.Registry = deps.Registry
	}
	if cfg.Deps.Hosts == nil {
		cfg.Deps.Hosts = deps.Hosts
	}
	if cfg.Deps.TLS == nil {
		cfg.Deps.TLS = deps.TLS
	}
	if cfg.Deps.Log == nil {
		cfg.Deps.Log = deps.Log
	}
	if cfg.StateDir == "" {
		cfg.StateDir = deps.StateDir
	}
	if cfg.StateDir == "" {
		return errors.New("pds: a state directory is required")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("pds: creating the state directory: %w", err)
	}
	credentials := deps.Credentials
	if credentials == nil {
		credentials = cfg.Credentials
	}
	if _, err := buildTLSConfig(cfg); err != nil {
		return err
	}
	var err error
	var (
		repoStore    *repo.SQLiteStore
		accountStore *atpds.Store
	)
	success := false
	cleanup := func() {
		if accountStore != nil {
			_ = accountStore.Close()
		}
		if repoStore != nil {
			_ = repoStore.Close()
		}
	}
	defer func() {
		if !success {
			cleanup()
		}
	}()

	key := cfg.Key
	if key.Kind == "" {
		key.Kind = Kind
	}
	if key.Name == "" {
		key.Name = "default"
	}
	logicalURL := "http://" + logicalHost(key)
	baseURL := cfg.PublicBaseURL
	if baseURL == "" {
		baseURL = logicalURL
	}
	if cfg.PLCURL != "" && cfg.Deps.Log != nil {
		cfg.Deps.Log.Info("pds plc directory", "url", cfg.PLCURL, "publicBaseURL", baseURL)
	}
	handlers := cfg.Deps.Handlers
	if cfg.PLCClient == nil && cfg.PLCURL != "" {
		if handlers != nil && strings.HasPrefix(strings.ToLower(cfg.PLCURL), "http://") && !isLoopbackURL(cfg.PLCURL) {
			cfg.PLCClient = handlers.HTTPClient()
		} else {
			cfg.PLCClient = cfg.Deps.ExternalHTTPClient(directoryTimeout)
		}
	}

	repoStore, err = repo.NewSQLiteStore(filepath.Join(cfg.StateDir, "repos.db"))
	if err != nil {
		return fmt.Errorf("pds: opening the repo store: %w", err)
	}
	if cfg.Mode == atpds.ModeFull {
		accountStore, err = atpds.NewStore(filepath.Join(cfg.StateDir, "accounts.db"))
		if err != nil {
			return fmt.Errorf("pds: opening the account store: %w", err)
		}
	}
	blobStore := cfg.Blobs
	if blobStore == nil {
		blobDir := cfg.BlobsDir
		if blobDir == "" {
			blobDir = filepath.Join(cfg.StateDir, "blobs")
		}
		blobStore, err = NewFileBlobStore(blobDir)
		if err != nil {
			return err
		}
	}

	var sessionSecret []byte
	if cfg.Mode == atpds.ModeFull {
		sessionSecret, err = resolveSessionSecret(ctx, cfg.StateDir, cfg.SessionSecret, cfg.SessionSecretPath, cfg.VaultRef, credentials)
		if err != nil {
			return err
		}
	}

	keyStore, err := resolveRepoKeyStore(ctx, cfg, credentials)
	if err != nil {
		return err
	}
	if cfg.Mode == atpds.ModeFull && keyStore == nil {
		return errors.New("pds: full mode needs a repo key store (repoKeyDir or a credential ref)")
	}

	directory := cfg.Directory
	var resolver atpds.DIDResolver = cfg.Resolver
	if directory == nil && resolver == nil {
		directory = buildDirectory(cfg.PLCURL, cfg.PLCClient, handlers, cfg.Deps)
	}

	submitter := cfg.PLC
	rotation := cfg.RotationKey
	if cfg.Mode == atpds.ModeFull && submitter == nil && cfg.PLCURL != "" {
		rotation, err = resolveRotationKey(ctx, cfg.StateDir, rotation, cfg.RotationKeyPath, cfg.VaultRef, nil, "", credentials)
		if err != nil {
			return err
		}
		submitter, err = NewHTTPPLCSubmitter(cfg.PLCURL, rotation, cfg.PLCClient)
		if err != nil {
			return err
		}
	}
	if cfg.Mode == atpds.ModeFull && submitter == nil && !cfg.AllowExplicitDID {
		return errors.New("pds: full mode needs a PLC directory URL or submitter")
	}
	rotationDID := ""
	if rotation != nil {
		pub, err := rotation.PublicKey()
		if err != nil {
			return fmt.Errorf("pds: deriving the rotation key: %w", err)
		}
		rotationDID = pub.DIDKey()
	}

	oauthConfig, err := s.oauthConfig(ctx, cfg, baseURL, credentials)
	if err != nil {
		return err
	}

	server, err := atpds.New(atpds.Config{
		Mode:                 cfg.Mode,
		Repos:                repoStore,
		Store:                accountStore,
		KeyStore:             keyStore,
		Resolver:             resolver,
		Directory:            directory,
		PLC:                  submitter,
		Blobs:                blobStore,
		OAuth:                oauthConfig,
		ServiceDID:           cfg.ServiceDID,
		PublicBaseURL:        baseURL,
		AlsoKnownAs:          append([]string(nil), cfg.AlsoKnownAs...),
		RotationKey:          rotationDID,
		AvailableUserDomains: append([]string(nil), cfg.AvailableUserDomains...),
		ContactEmail:         cfg.ContactEmail,
		Version:              cfg.Version,
		SessionSecret:        sessionSecret,
		ServiceAuthAudiences: append([]string(nil), cfg.ServiceAuthAudiences...),
		MaxBlobSize:          cfg.MaxBlobSize,
		MaxRequestBytes:      cfg.MaxRequestBytes,
		AllowExplicitDID:     cfg.AllowExplicitDID,
	})
	if err != nil {
		return err
	}

	if handlers == nil {
		return errors.New("pds: no handler registry is available")
	}
	if err := handlers.RegisterHTTP(key, server.Handler()); err != nil {
		return fmt.Errorf("pds: registering the handler: %w", err)
	}
	stopCh := make(chan struct{})
	endpoint := component.Endpoint{
		Name:     "http",
		Protocol: component.ProtocolHTTP,
		URL:      logicalURL,
	}

	s.mu.Lock()
	s.cfg = cfg
	s.started = true
	s.endpoint = endpoint
	s.key = key
	s.baseURL = logicalURL
	s.stopCh = stopCh
	s.stopOnce = sync.Once{}
	s.server = server
	s.repoStore = repoStore
	s.accountStore = accountStore
	s.keyStore = keyStore
	s.mu.Unlock()

	success = true
	if cfg.RequireIngress && cfg.PublicBaseURL != "" && cfg.Mode == atpds.ModeFull && submitter != nil {
		go s.syncServiceEndpoints(ctx, accountStore, submitter, cfg.PublicBaseURL)
	}
	select {
	case <-ctx.Done():
	case <-stopCh:
	}
	s.mu.Lock()
	s.started = false
	s.stopCh = nil
	s.mu.Unlock()
	return nil
}

func (s *Service) syncServiceEndpoints(ctx context.Context, store *atpds.Store, submitter atpds.PLCSubmitter, publicURL string) {
	if store == nil || submitter == nil || strings.TrimSpace(publicURL) == "" {
		return
	}
	dids, err := store.ListDIDs(ctx)
	if err != nil {
		return
	}
	for _, did := range dids {
		if !strings.HasPrefix(string(did), "did:plc:") {
			continue
		}
		if err := submitter.UpdateService(ctx, did, publicURL); err != nil && s.cfg.Deps.Log != nil {
			s.cfg.Deps.Log.Warn("syncing the PDS service endpoint", "did", did.String(), "err", err.Error())
		}
	}
}

func (s *Service) oauthConfig(ctx context.Context, cfg Config, baseURL string, credentials component.CredentialResolver) (*atpds.OAuthConfig, error) {
	if cfg.Mode != atpds.ModeFull || cfg.OAuth == nil {
		return nil, nil
	}
	opts := cfg.OAuth
	signerKey, err := resolveOAuthSigner(ctx, cfg.StateDir, opts.Signer, opts.SignerRef, opts.SignerField, credentials)
	if err != nil {
		return nil, err
	}
	serviceDID := cfg.ServiceDID
	if serviceDID == "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Host == "" {
			return nil, errors.New("pds: cannot derive the service DID")
		}
		serviceDID = "did:web:" + url.QueryEscape(parsed.Host)
	}
	signerDID, err := syntax.ParseDID(serviceDID)
	if err != nil {
		return nil, fmt.Errorf("pds: invalid service DID: %w", err)
	}
	issuer := opts.Issuer
	if issuer == "" {
		issuer = baseURL
	}
	return &atpds.OAuthConfig{
		Issuer:                  issuer,
		Signer:                  oauth.Signer{DID: signerDID, Key: signerKey},
		HTTPClient:              opts.HTTPClient,
		AccessTokenTTL:          opts.AccessTokenTTL,
		RefreshTokenTTL:         opts.RefreshTokenTTL,
		AuthorizationCodeTTL:    opts.AuthorizationCodeTTL,
		PARTTL:                  opts.PARTTL,
		AllowMissingDPoPNonce:   opts.AllowMissingDPoPNonce,
		DisableLocalhostClients: opts.DisableLocalhostClients,
		Scopes:                  append([]string(nil), opts.Scopes...),
	}, nil
}

func buildDirectory(plcURL string, client *http.Client, handlers *component.HandlerRegistry, deps component.Deps) identity.Directory {
	if plcURL == "" {
		return identity.DefaultDirectory()
	}
	if handlers != nil && strings.HasPrefix(strings.ToLower(plcURL), "http://") && !isLoopbackURL(plcURL) {
		httpClient := handlers.HTTPClient()
		return &identity.BaseDirectory{
			PLCURL:     plcURL,
			HTTPClient: *httpClient,
			PLCClient:  httpClient,
		}
	}
	plcClient := client
	if plcClient == nil {
		plcClient = deps.ExternalHTTPClient(directoryTimeout)
	}
	return &identity.BaseDirectory{
		PLCURL: plcURL,
		HTTPClient: http.Client{
			Timeout:   directoryTimeout,
			Transport: ssrf.PublicOnlyTransport(),
		},
		PLCClient: plcClient,
	}
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func logicalHost(key component.Key) string {
	kind := strings.ToLower(strings.TrimSpace(key.Kind))
	if kind == "" {
		return ""
	}
	if name := strings.ToLower(strings.TrimSpace(key.Name)); name != "" {
		return kind + "." + name
	}
	return kcp.ServiceFQDN(kind, "default", key.Workspace, kcp.DefaultServiceDomain)
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	rawURL := s.endpoint.URL
	handlers := s.cfg.Deps.Handlers
	s.mu.Unlock()
	if !started || rawURL == "" || handlers == nil {
		return errors.New("pds: the service is not registered")
	}
	probeCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := xrpc.Health(probeCtx, &xrpc.Client{Host: rawURL, HTTP: handlers.HTTPClient()}); err != nil {
		return fmt.Errorf("pds: health: %w", err)
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.endpoint.URL == "" {
		return nil
	}
	return []component.Endpoint{s.endpoint}
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	stopCh := s.stopCh
	s.mu.Unlock()
	if stopCh != nil {
		s.stopOnce.Do(func() { close(stopCh) })
	}
	s.closeResources()
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
	return nil
}

func (s *Service) closeResources() {
	s.closeOnce.Do(func() {
		if s.accountStore != nil {
			_ = s.accountStore.Close()
		}
		if s.repoStore != nil {
			_ = s.repoStore.Close()
		}
	})
}

func (s *Service) Server() *atpds.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server
}

func (s *Service) KeyStore() atpds.RepoKeyStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyStore
}

func (s *Service) ServiceDID() string {
	server := s.Server()
	if server == nil {
		return ""
	}
	return server.ServiceDID()
}

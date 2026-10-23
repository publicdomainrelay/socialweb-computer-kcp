package pds

import (
	"cmp"
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

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/bluesky-social/indigo/util/ssrf"
	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/plcsubmit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
)

const (
	shutdownTimeout  = 10 * time.Second
	directoryTimeout = 10 * time.Second
)

type Service struct {
	spec v1alpha1.PdsSpec
	deps Deps

	mode          atpds.Mode
	stateDir      string
	publicBaseURL string
	plcURL        string

	mu       sync.Mutex
	started  bool
	endpoint component.Endpoint
	key      component.Key
	baseURL  string
	server   *atpds.Server
	stopCh   chan struct{}
	stopOnce sync.Once

	readiness component.ReadySignal

	closeOnce    sync.Once
	repoStore    *repo.SQLiteStore
	accountStore *atpds.Store
	keyStore     atpds.RepoKeyStore
}

var _ component.Service = (*Service)(nil)

func New(spec v1alpha1.PdsSpec, deps Deps) (*Service, error) {
	s := &Service{
		spec:          spec,
		deps:          deps,
		mode:          atpds.ModeFull,
		stateDir:      strings.TrimSpace(deps.StateDir),
		publicBaseURL: strings.TrimRight(strings.TrimSpace(spec.PublicBaseURL), "/"),
		plcURL:        strings.TrimRight(strings.TrimSpace(spec.PlcURL), "/"),
	}
	if spec.Mode == v1alpha1.PdsModeRepo {
		s.mode = atpds.ModeRepo
	}
	if s.mode == atpds.ModeRepo && spec.OAuth != nil {
		return nil, errors.New("pds: oauth is not available in read-only repo mode")
	}
	if spec.Storage != nil {
		s.stateDir = cmp.Or(strings.TrimSpace(spec.Storage.DataDir), s.stateDir)
	}
	if s.plcURL == "" && spec.Network != nil {
		s.plcURL = strings.TrimRight(strings.TrimSpace(spec.Network.PLCDirectoryURL), "/")
	}
	if spec.RequireIngress && s.publicBaseURL != "" {
		if err := publicEndpoint(s.publicBaseURL); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Service) componentPath(name string) string {
	if s.deps.StateDir == "" {
		return ""
	}
	return filepath.Join(s.deps.StateDir, name)
}

func (s *Service) serviceDID() string {
	if s.spec.Identity == nil {
		return ""
	}
	return strings.TrimSpace(s.spec.Identity.Did)
}

func (s *Service) alsoKnownAs() []string {
	if s.spec.Identity == nil {
		return nil
	}
	if handle := strings.TrimSpace(s.spec.Identity.Handle); handle != "" {
		return []string{"at://" + handle}
	}
	return nil
}

func (s *Service) serviceAuthAudiences() []string {
	if s.spec.ServiceAuth == nil {
		return nil
	}
	return append([]string(nil), s.spec.ServiceAuth.Audience...)
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

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("pds: the service is already started")
	}
	s.mu.Unlock()

	stateDir := s.stateDir
	if stateDir == "" {
		return errors.New("pds: a state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("pds: creating the state directory: %w", err)
	}
	log := s.deps.Log
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

	key := s.deps.Key()
	key.Kind = cmp.Or(key.Kind, Kind)
	key.Name = cmp.Or(key.Name, "default")
	logicalURL := "http://" + logicalHost(key)
	baseURL := cmp.Or(s.publicBaseURL, logicalURL)
	plcURL := s.plcURL
	if plcURL != "" && log != nil {
		log.Info("pds plc directory", "url", plcURL, "publicBaseURL", baseURL)
	}
	handlers := s.deps.Bus
	var plcClient *http.Client
	if plcURL != "" {
		if handlers != nil && strings.HasPrefix(strings.ToLower(plcURL), "http://") && !isLoopbackURL(plcURL) {
			plcClient = handlers.HTTPClient()
		} else {
			plcClient = s.deps.Egress.Client(directoryTimeout)
		}
	}

	repoStore, err = repo.NewSQLiteStore(filepath.Join(stateDir, "repos.db"))
	if err != nil {
		return fmt.Errorf("pds: opening the repo store: %w", err)
	}
	if s.mode == atpds.ModeFull {
		accountStore, err = atpds.NewStore(filepath.Join(stateDir, "accounts.db"))
		if err != nil {
			return fmt.Errorf("pds: opening the account store: %w", err)
		}
	}
	blobDir := filepath.Join(stateDir, "blobs")
	if s.spec.Storage != nil {
		blobDir = cmp.Or(strings.TrimSpace(s.spec.Storage.BlobsDir), blobDir)
	}
	blobStore, err := files.NewFileBlobStore(blobDir)
	if err != nil {
		return err
	}

	var sessionSecret []byte
	if s.mode == atpds.ModeFull {
		sessionSecret, err = files.SessionSecret(stateDir, s.componentPath(SessionSecretFile))
		if err != nil {
			return err
		}
	}

	keyStore, err := files.RepoKeyStore(s.componentPath(RepoKeyDir))
	if err != nil {
		return err
	}
	if s.mode == atpds.ModeFull && keyStore == nil {
		return errors.New("pds: full mode needs a repo key store (a state dir for repo-keys)")
	}

	directory := buildDirectory(plcURL, plcClient, handlers, s.deps.Egress)

	var submitter atpds.PLCSubmitter
	var rotation atcrypto.PrivateKey
	if s.mode == atpds.ModeFull && plcURL != "" {
		rotation, err = files.RotationKey(stateDir, s.componentPath(RotationKeyFile))
		if err != nil {
			return err
		}
		submitter, err = plcsubmit.New(plcURL, rotation, plcClient)
		if err != nil {
			return err
		}
	}
	if s.mode == atpds.ModeFull && submitter == nil && !s.spec.AllowExplicitDID {
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

	oauthConfig, err := s.oauthConfig(stateDir, baseURL)
	if err != nil {
		return err
	}

	server, err := atpds.New(atpds.Config{
		Mode:                 s.mode,
		Repos:                repoStore,
		Store:                accountStore,
		KeyStore:             keyStore,
		Directory:            directory,
		PLC:                  submitter,
		Blobs:                blobStore,
		OAuth:                oauthConfig,
		ServiceDID:           s.serviceDID(),
		PublicBaseURL:        baseURL,
		AlsoKnownAs:          s.alsoKnownAs(),
		RotationKey:          rotationDID,
		ContactEmail:         strings.TrimSpace(s.spec.ContactEmail),
		SessionSecret:        sessionSecret,
		ServiceAuthAudiences: s.serviceAuthAudiences(),
		MaxBlobSize:          ptr.Deref(s.spec.MaxBlobSize, 0),
		MaxRequestBytes:      ptr.Deref(s.spec.MaxRequestBytes, 0),
		AllowExplicitDID:     s.spec.AllowExplicitDID,
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
	s.readiness.Signal()

	success = true
	syncCtx, cancelSync := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if s.spec.RequireIngress && s.publicBaseURL != "" && s.mode == atpds.ModeFull && submitter != nil {
		wg.Go(func() { s.syncServiceEndpoints(syncCtx, accountStore, submitter, s.publicBaseURL) })
	}
	select {
	case <-ctx.Done():
	case <-stopCh:
	}
	cancelSync()
	wg.Wait()
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
		if err := submitter.UpdateService(ctx, did, publicURL); err != nil && s.deps.Log != nil {
			s.deps.Log.Warn("syncing the PDS service endpoint", "did", did.String(), "err", err.Error())
		}
	}
}

func (s *Service) oauthConfig(stateDir, baseURL string) (*atpds.OAuthConfig, error) {
	if s.mode != atpds.ModeFull || s.spec.OAuth == nil {
		return nil, nil
	}
	opts := s.spec.OAuth
	signerKey, err := files.OAuthSigner(stateDir)
	if err != nil {
		return nil, err
	}
	serviceDID := s.serviceDID()
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
	issuer := cmp.Or(strings.TrimRight(strings.TrimSpace(opts.Issuer), "/"), baseURL)
	return &atpds.OAuthConfig{
		Issuer:                  issuer,
		Signer:                  oauth.Signer{DID: signerDID, Key: signerKey},
		AccessTokenTTL:          time.Duration(opts.AccessTokenTTLSeconds) * time.Second,
		RefreshTokenTTL:         time.Duration(opts.RefreshTokenTTLSeconds) * time.Second,
		DisableLocalhostClients: opts.DisableLocalhostClients || !opts.AllowLocalhostClients,
		Scopes:                  append([]string(nil), opts.Scopes...),
	}, nil
}

func buildDirectory(plcURL string, client *http.Client, handlers *bus.Bus, egress component.Egress) identity.Directory {
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
		plcClient = egress.Client(directoryTimeout)
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

func (s *Service) Ready() component.Readiness {
	return &s.readiness
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

func (s *Service) UpdateService(ctx context.Context, did syntax.DID, service string) error {
	server := s.Server()
	if server == nil {
		return errors.New("pds: the service is not started")
	}
	return server.UpdateService(ctx, did, service)
}

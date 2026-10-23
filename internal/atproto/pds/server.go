package pds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
)

const (
	DefaultAccessTokenTTL  = 2 * time.Hour
	DefaultRefreshTokenTTL = 30 * 24 * time.Hour
	DefaultMaxBlobSize     = 5 << 20
	DefaultMaxRequestBytes = 4 << 20
)

type Server struct {
	cfg             Config
	mode            Mode
	repos           repo.RepoStore
	store           *Store
	keys            RepoKeyStore
	resolver        DIDResolver
	plc             PLCSubmitter
	blobs           BlobStore
	seq             *Sequencer
	oauthServer     *oauth.Server
	verifier        *serviceauth.Verifier
	mux             *http.ServeMux
	handler         http.Handler
	sessionSecret   []byte
	serviceDID      string
	audience        string
	accessTTL       time.Duration
	refreshTTL      time.Duration
	maxRequestBytes int64
	maxBlobSize     int64
}

func New(cfg Config) (*Server, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeFull
	}
	if mode != ModeFull && mode != ModeRepo {
		return nil, fmt.Errorf("pds: unknown mode %q", mode)
	}
	if cfg.Repos == nil {
		return nil, fmt.Errorf("pds: repo store is required")
	}
	if mode == ModeFull && cfg.Store == nil {
		return nil, fmt.Errorf("pds: account store is required in full mode")
	}
	if mode == ModeFull && cfg.KeyStore == nil {
		return nil, fmt.Errorf("pds: repo key store is required in full mode")
	}
	if mode == ModeRepo && cfg.OAuth != nil {
		return nil, fmt.Errorf("pds: oauth is not available in read-only repo mode")
	}
	secret := append([]byte(nil), cfg.SessionSecret...)
	if mode == ModeFull && len(secret) == 0 {
		return nil, fmt.Errorf("pds: session secret is required in full mode")
	}
	serviceDID := cfg.ServiceDID
	if serviceDID != "" {
		if _, err := syntax.ParseDID(serviceDID); err != nil {
			return nil, fmt.Errorf("pds: invalid service DID: %w", err)
		}
	} else if cfg.PublicBaseURL != "" {
		u, err := url.Parse(cfg.PublicBaseURL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("pds: invalid public base URL")
		}
		serviceDID = "did:web:" + url.QueryEscape(u.Host)
		if _, err := syntax.ParseDID(serviceDID); err != nil {
			return nil, fmt.Errorf("pds: invalid derived service DID: %w", err)
		}
	} else {
		serviceDID = "did:web:localhost"
	}
	audience := cfg.PublicBaseURL
	if serviceDID != "" {
		audience = serviceDID
	}
	if audience == "" {
		audience = "did:web:localhost"
	}
	accessTTL := cfg.AccessTokenTTL
	if accessTTL <= 0 {
		accessTTL = DefaultAccessTokenTTL
	}
	refreshTTL := cfg.RefreshTokenTTL
	if refreshTTL <= 0 {
		refreshTTL = DefaultRefreshTokenTTL
	}
	maxRequest := cfg.MaxRequestBytes
	if maxRequest <= 0 {
		maxRequest = DefaultMaxRequestBytes
	}
	maxBlob := cfg.MaxBlobSize
	if maxBlob <= 0 {
		maxBlob = DefaultMaxBlobSize
	}
	s := &Server{
		cfg:             cfg,
		mode:            mode,
		repos:           cfg.Repos,
		store:           cfg.Store,
		keys:            cfg.KeyStore,
		plc:             cfg.PLC,
		blobs:           cfg.Blobs,
		sessionSecret:   secret,
		serviceDID:      serviceDID,
		audience:        audience,
		accessTTL:       accessTTL,
		refreshTTL:      refreshTTL,
		maxRequestBytes: maxRequest,
		maxBlobSize:     maxBlob,
		mux:             http.NewServeMux(),
	}
	if mode == ModeRepo {
		s.store = nil
	}
	if cfg.Resolver != nil {
		s.resolver = cfg.Resolver
	} else if cfg.Directory != nil {
		s.resolver = DirectoryResolver{Directory: cfg.Directory}
	}
	if cfg.Directory != nil {
		audiences := append([]string(nil), cfg.ServiceAuthAudiences...)
		if len(audiences) == 0 {
			audiences = []string{audience}
			if cfg.PublicBaseURL != "" && cfg.PublicBaseURL != audience {
				audiences = append(audiences, strings.TrimRight(cfg.PublicBaseURL, "/"))
			}
		}
		s.verifier = &serviceauth.Verifier{Audiences: audiences, Directory: cfg.Directory}
	}
	s.seq = NewSequencer()
	cfg.Repos.SetEventSink(s.seq)
	s.registerRoutes()
	if cfg.OAuth != nil {
		if err := s.initOAuth(cfg.OAuth); err != nil {
			return nil, err
		}
	}
	var handler http.Handler = s.mux
	if s.oauthServer != nil {
		resource := s.oauthServer.ResourceHandler(s.mux)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/oauth/") || strings.HasPrefix(r.URL.Path, "/.well-known/") {
				s.mux.ServeHTTP(w, r)
				return
			}
			resource.ServeHTTP(w, r)
		})
	}
	if mode == ModeRepo {
		handler = s.readOnlyGuard(handler)
	}
	s.handler = handler
	return s, nil
}

func (s *Server) initOAuth(cfg *OAuthConfig) error {
	if s.store == nil {
		return fmt.Errorf("pds: oauth requires an account store")
	}
	issuer := cfg.Issuer
	if issuer == "" {
		issuer = s.cfg.PublicBaseURL
	}
	if issuer == "" {
		return fmt.Errorf("pds: oauth issuer is required")
	}
	signer := cfg.Signer
	if signer.DID == "" {
		did, err := syntax.ParseDID(s.serviceDID)
		if err != nil {
			return fmt.Errorf("pds: oauth signer DID is required")
		}
		signer.DID = did
	}
	if signer.Key == nil {
		return fmt.Errorf("pds: oauth signer key is required")
	}
	server, err := oauth.New(oauth.Config{
		Issuer:                  issuer,
		Signer:                  signer,
		Accounts:                s.store,
		Sessions:                s.store,
		Codes:                   s.store,
		Tokens:                  s.store,
		PARs:                    s.store,
		Nonces:                  oauth.NewMemoryNonceStore(),
		HTTPClient:              cfg.HTTPClient,
		AccessTokenTTL:          cfg.AccessTokenTTL,
		RefreshTokenTTL:         cfg.RefreshTokenTTL,
		AuthorizationCodeTTL:    cfg.AuthorizationCodeTTL,
		PARTTL:                  cfg.PARTTL,
		AllowMissingDPoPNonce:   cfg.AllowMissingDPoPNonce,
		DisableLocalhostClients: cfg.DisableLocalhostClients,
		Scopes:                  append([]string(nil), cfg.Scopes...),
	})
	if err != nil {
		return fmt.Errorf("pds: initializing oauth: %w", err)
	}
	server.Register(s.mux)
	s.oauthServer = server
	return nil
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /_health", s.handleHealth)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /xrpc/_health", s.handleHealth)
	s.mux.HandleFunc("GET /.well-known/atproto-did", s.handleWellKnownDID)
	s.mux.HandleFunc("GET /.well-known/did.json", s.handleWellKnownDIDDocument)

	s.mux.HandleFunc("GET /xrpc/com.atproto.server.describeServer", s.handleDescribeServer)
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.createAccount", s.handleCreateAccount)
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.createSession", s.handleCreateSession)
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.refreshSession", s.handleRefreshSession)
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.deleteSession", s.handleDeleteSession)
	s.mux.HandleFunc("GET /xrpc/com.atproto.server.getSession", s.requireSession(s.handleGetSession))
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.createAppPassword", s.requireSession(s.handleCreateAppPassword))
	s.mux.HandleFunc("GET /xrpc/com.atproto.server.listAppPasswords", s.requireSession(s.handleListAppPasswords))
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.revokeAppPassword", s.requireSession(s.handleRevokeAppPassword))
	s.mux.HandleFunc("GET /xrpc/com.atproto.server.getServiceAuth", s.requireSession(s.handleGetServiceAuth))
	s.mux.HandleFunc("POST /xrpc/com.atproto.server.getServiceAuth", s.requireSession(s.handleGetServiceAuth))

	s.mux.HandleFunc("GET /xrpc/com.atproto.repo.getRecord", s.handleGetRecord)
	s.mux.HandleFunc("GET /xrpc/com.atproto.repo.listRecords", s.handleListRecords)
	s.mux.HandleFunc("GET /xrpc/com.atproto.repo.describeRepo", s.handleDescribeRepo)
	s.mux.HandleFunc("POST /xrpc/com.atproto.repo.createRecord", s.requireSession(s.handleCreateRecord))
	s.mux.HandleFunc("POST /xrpc/com.atproto.repo.putRecord", s.requireSession(s.handlePutRecord))
	s.mux.HandleFunc("POST /xrpc/com.atproto.repo.deleteRecord", s.requireSession(s.handleDeleteRecord))
	s.mux.HandleFunc("POST /xrpc/com.atproto.repo.applyWrites", s.requireSession(s.handleApplyWrites))
	s.mux.HandleFunc("POST /xrpc/com.atproto.repo.uploadBlob", s.requireSession(s.handleUploadBlob))

	s.mux.HandleFunc("GET /xrpc/com.atproto.sync.getRepo", s.handleGetRepo)
	s.mux.HandleFunc("GET /xrpc/com.atproto.sync.getLatestCommit", s.handleGetLatestCommit)
	s.mux.HandleFunc("GET /xrpc/com.atproto.sync.getRepoStatus", s.handleGetRepoStatus)
	s.mux.HandleFunc("GET /xrpc/com.atproto.repo.getRepoStatus", s.handleGetRepoStatus)
	s.mux.HandleFunc("GET /xrpc/com.atproto.sync.getRecord", s.handleSyncGetRecord)
	s.mux.HandleFunc("GET /xrpc/com.atproto.sync.getBlob", s.handleGetBlob)
	s.mux.HandleFunc("GET /xrpc/com.atproto.sync.subscribeRepos", s.handleSubscribeRepos)

	s.mux.HandleFunc("/xrpc/", s.handleMethodNotFound)
}

var readOnlyBlocked = map[string]bool{
	"/xrpc/com.atproto.server.createAccount":     true,
	"/xrpc/com.atproto.server.createSession":     true,
	"/xrpc/com.atproto.server.refreshSession":    true,
	"/xrpc/com.atproto.server.deleteSession":     true,
	"/xrpc/com.atproto.server.getSession":        true,
	"/xrpc/com.atproto.server.createAppPassword": true,
	"/xrpc/com.atproto.server.listAppPasswords":  true,
	"/xrpc/com.atproto.server.revokeAppPassword": true,
	"/xrpc/com.atproto.server.getServiceAuth":    true,
	"/xrpc/com.atproto.repo.createRecord":        true,
	"/xrpc/com.atproto.repo.putRecord":           true,
	"/xrpc/com.atproto.repo.deleteRecord":        true,
	"/xrpc/com.atproto.repo.applyWrites":         true,
	"/xrpc/com.atproto.repo.uploadBlob":          true,
}

func (s *Server) readOnlyGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if readOnlyBlocked[r.URL.Path] {
			writeXRPCError(w, http.StatusForbidden, "Forbidden", "PDS is running in read-only repo mode")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) Mode() Mode {
	return s.mode
}

func (s *Server) Sequencer() *Sequencer {
	return s.seq
}

func (s *Server) Repos() repo.RepoStore {
	return s.repos
}

func (s *Server) Store() *Store {
	return s.store
}

func (s *Server) KeyStore() RepoKeyStore {
	return s.keys
}

func (s *Server) OAuthServer() *oauth.Server {
	return s.oauthServer
}

func (s *Server) ServiceDID() string {
	return s.serviceDID
}

func (s *Server) UpdateService(ctx context.Context, did syntax.DID, service string) error {
	if s.plc == nil {
		return ErrUnsupported
	}
	return s.plc.UpdateService(ctx, did, service)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	version := s.cfg.Version
	if version == "" {
		version = "socialweb-pds"
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": version})
}

func (s *Server) handleMethodNotFound(w http.ResponseWriter, _ *http.Request) {
	writeXRPCError(w, http.StatusNotFound, "MethodNotFound", "unknown xrpc method")
}

type xrpcErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func writeXRPCError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, xrpcErrorBody{Error: code, Message: message})
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) repoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repo.ErrInvalidRequest):
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, repo.ErrInvalidSwap):
		writeXRPCError(w, http.StatusBadRequest, "InvalidSwap", err.Error())
	case errors.Is(err, repo.ErrRecordNotFound):
		writeXRPCError(w, http.StatusBadRequest, "RecordNotFound", err.Error())
	case errors.Is(err, repo.ErrRecordExists):
		writeXRPCError(w, http.StatusBadRequest, "RecordAlreadyExists", err.Error())
	case errors.Is(err, repo.ErrRepoNotFound), errors.Is(err, ErrAccountNotFound):
		writeXRPCError(w, http.StatusBadRequest, "RepoNotFound", "repo not found")
	case errors.Is(err, repo.ErrReadOnly):
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "repo is read-only")
	default:
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "internal server error")
	}
}

func (s *Server) openRepo(ctx context.Context, did syntax.DID) (repo.Repo, error) {
	if s.store == nil {
		return nil, repo.ErrReadOnly
	}
	if s.keys == nil {
		return nil, repo.ErrReadOnly
	}
	key, err := s.keys.GetRepoKey(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("pds: loading repo signing key: %w", err)
	}
	return s.repos.OpenRepo(ctx, did, key)
}

func recordType(record map[string]any, collection string) map[string]any {
	if record == nil {
		return nil
	}
	if _, ok := record["$type"]; ok {
		return record
	}
	out := make(map[string]any, len(record)+1)
	out["$type"] = collection
	for k, v := range record {
		out[k] = v
	}
	return out
}

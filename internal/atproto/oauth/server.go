package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	DefaultAccessTokenTTL       = 15 * time.Minute
	DefaultRefreshTokenTTL      = 7 * 24 * time.Hour
	DefaultAuthorizationCodeTTL = 5 * time.Minute
	DefaultPARTTL               = 60 * time.Second

	MaxOAuthFormBytes = 1 << 20
)

type Config struct {
	Issuer string
	Signer Signer

	Accounts AccountStore
	Sessions SessionStore
	Codes    AuthorizationCodeStore
	Tokens   TokenStore
	PARs     PARStore
	Nonces   NonceStore

	HTTPClient *http.Client

	AccessTokenTTL       time.Duration
	RefreshTokenTTL      time.Duration
	AuthorizationCodeTTL time.Duration
	PARTTL               time.Duration
	ClientMetadataTTL    time.Duration
	DPoPLeeway           time.Duration

	AllowMissingDPoPNonce   bool
	DisableLocalhostClients bool

	Scopes []string
}

type Server struct {
	issuer   string
	signer   Signer
	accounts AccountStore
	sessions SessionStore
	codes    AuthorizationCodeStore
	tokens   TokenStore
	pars     PARStore
	nonces   NonceStore
	config   Config
	dpop     *DPoPVerifier
	clients  *ClientMetadataResolver
}

func New(cfg Config) (*Server, error) {
	issuer, err := normalizeIssuer(cfg.Issuer)
	if err != nil {
		return nil, err
	}
	if cfg.Signer.DID == "" {
		return nil, fmt.Errorf("oauth: signer DID is required")
	}
	if _, err := syntax.ParseDID(cfg.Signer.DID.String()); err != nil {
		return nil, fmt.Errorf("oauth: invalid signer DID: %w", err)
	}
	if cfg.Signer.Key == nil {
		return nil, fmt.Errorf("oauth: signer key is required")
	}
	if _, err := signingAlgorithm(cfg.Signer.Key); err != nil {
		return nil, err
	}
	if cfg.Accounts == nil || cfg.Sessions == nil || cfg.Codes == nil || cfg.Tokens == nil || cfg.PARs == nil {
		return nil, fmt.Errorf("oauth: all stores are required")
	}
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = DefaultAccessTokenTTL
	}
	if cfg.RefreshTokenTTL <= 0 {
		cfg.RefreshTokenTTL = DefaultRefreshTokenTTL
	}
	if cfg.AuthorizationCodeTTL <= 0 {
		cfg.AuthorizationCodeTTL = DefaultAuthorizationCodeTTL
	}
	if cfg.PARTTL <= 0 {
		cfg.PARTTL = DefaultPARTTL
	}
	if cfg.ClientMetadataTTL <= 0 {
		cfg.ClientMetadataTTL = DefaultClientMetadataTTL
	}
	if cfg.DPoPLeeway <= 0 {
		cfg.DPoPLeeway = DefaultDPoPLeeway
	}
	if cfg.Nonces == nil {
		cfg.Nonces = NewMemoryNonceStore()
	}

	verifier := NewDPoPVerifier(cfg.Nonces)
	verifier.Leeway = cfg.DPoPLeeway
	verifier.RequireNonce = !cfg.AllowMissingDPoPNonce

	resolver := NewClientMetadataResolver(cfg.Nonces)
	resolver.Client = hardenHTTPClient(cfg.HTTPClient)
	resolver.CacheTTL = cfg.ClientMetadataTTL
	resolver.AllowLocalhost = !cfg.DisableLocalhostClients

	return &Server{
		issuer:   issuer,
		signer:   cfg.Signer,
		accounts: cfg.Accounts,
		sessions: cfg.Sessions,
		codes:    cfg.Codes,
		tokens:   cfg.Tokens,
		pars:     cfg.PARs,
		nonces:   cfg.Nonces,
		config:   cfg,
		dpop:     verifier,
		clients:  resolver,
	}, nil
}

func normalizeIssuer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("oauth: issuer is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("oauth: invalid issuer: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("oauth: issuer must be an http or https URL")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("oauth: issuer must be an origin")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("oauth: issuer must not have a path")
	}
	if scheme == "http" && !isLoopbackHostname(parsed.Hostname()) && !isInternalKcpHostname(parsed.Hostname()) {
		return "", fmt.Errorf("oauth: http issuer is only allowed on loopback or the internal kcp service domain")
	}
	return scheme + "://" + strings.ToLower(parsed.Host), nil
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isInternalKcpHostname(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "svc.kcp.local" || strings.HasSuffix(host, ".svc.kcp.local")
}

func (s *Server) Issuer() string {
	return s.issuer
}

func (s *Server) AuthorizationServerMetadata() AuthorizationServerMetadata {
	issuer := s.issuer
	scopes := s.config.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeAtproto}
	}
	return AuthorizationServerMetadata{
		Issuer:                                     issuer,
		AuthorizationEndpoint:                      issuer + "/oauth/authorize",
		TokenEndpoint:                              issuer + "/oauth/token",
		PushedAuthorizationRequestEndpoint:         issuer + "/oauth/par",
		RevocationEndpoint:                         issuer + "/oauth/revoke",
		RequirePushedAuthorizationRequests:         true,
		TokenEndpointAuthMethodsSupported:          []string{ClientAuthNone, ClientAuthPrivateKeyJWT},
		TokenEndpointAuthSigningAlgValuesSupported: []string{"ES256"},
		ResponseTypesSupported:                     []string{"code"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		DPoPSigningAlgValuesSupported:              []string{"ES256"},
		AuthorizationResponseISSParameterSupported: true,
		ClientIDMetadataDocumentSupported:          true,
		DPoPBoundAccessTokensSupported:             true,
		CodeChallengeMethodsSupported:              []string{"S256"},
		ScopesSupported:                            append([]string(nil), scopes...),
	}
}

func (s *Server) ProtectedResourceMetadata() ProtectedResourceMetadata {
	return ProtectedResourceMetadata{
		Resource:             s.issuer,
		AuthorizationServers: []string{s.issuer},
	}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleAuthorizationServerMetadata)
	mux.HandleFunc("POST /oauth/par", s.handlePAR)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorizePage)
	mux.HandleFunc("POST /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func (s *Server) handleAuthorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.AuthorizationServerMetadata())
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ProtectedResourceMetadata())
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

type oauthErrorBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, oauthErrorBody{Error: code, ErrorDescription: description})
}

func parseOAuthForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxOAuthFormBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Server) newNonce(ctx context.Context) (string, error) {
	return s.nonces.IssueNonce(ctx, s.issuer)
}

func (s *Server) verifyAuthorizationDPoP(w http.ResponseWriter, r *http.Request, accessToken string) (*DPoPProof, bool) {
	ctx := r.Context()
	nonce, err := s.newNonce(ctx)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return nil, false
	}
	w.Header().Set(HeaderDPoPNonce, nonce)
	requestURL := s.issuer + r.URL.EscapedPath()
	proof, err := s.dpop.Verify(ctx, r.Header.Get(HeaderDPoP), r.Method, requestURL, accessToken)
	if err != nil {
		switch {
		case errors.Is(err, ErrUseDPoPNonce):
			writeOAuthError(w, http.StatusBadRequest, "use_dpop_nonce", "")
		case errors.Is(err, ErrInvalidDPoPProof):
			writeOAuthError(w, http.StatusUnauthorized, "invalid_dpop_proof", "")
		default:
			writeOAuthError(w, http.StatusUnauthorized, "invalid_dpop_proof", "")
		}
		return nil, false
	}
	return proof, true
}

type sessionContextKey struct{}

func SessionFromContext(ctx context.Context) (*Session, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(*Session)
	return session, ok
}

func DIDFromContext(ctx context.Context) (syntax.DID, bool) {
	session, ok := SessionFromContext(ctx)
	if !ok || session == nil {
		return "", false
	}
	return session.Account.DID, true
}

func (s *Server) ResourceHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		scheme, token, ok := authCredentials(authorization)
		if !ok || !strings.EqualFold(scheme, AuthSchemeDPoP) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		proofHeader := r.Header.Get(HeaderDPoP)
		if proofHeader == "" {
			nonce, err := s.newNonce(ctx)
			if err != nil {
				writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
				return
			}
			w.Header().Set(HeaderDPoPNonce, nonce)
			writeResourceError(w, http.StatusUnauthorized, "use_dpop_nonce", "", nonce)
			return
		}
		requestURL := s.issuer + r.URL.EscapedPath()
		proof, err := s.dpop.Verify(ctx, proofHeader, r.Method, requestURL, token)
		if err != nil {
			nonce, nonceErr := s.newNonce(ctx)
			if nonceErr != nil {
				writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
				return
			}
			w.Header().Set(HeaderDPoPNonce, nonce)
			if errors.Is(err, ErrUseDPoPNonce) {
				writeResourceError(w, http.StatusUnauthorized, "use_dpop_nonce", "", nonce)
				return
			}
			writeResourceError(w, http.StatusUnauthorized, "invalid_dpop_proof", "", nonce)
			return
		}
		session, claims, err := s.validateAccessToken(ctx, token)
		if err != nil {
			writeResourceError(w, http.StatusUnauthorized, "invalid_token", "", "")
			return
		}
		if claims.CNF.JKT != proof.JKT || session.JKT != proof.JKT {
			writeResourceError(w, http.StatusUnauthorized, "invalid_token", "token not bound to this DPoP key", "")
			return
		}
		nonce, err := s.newNonce(ctx)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		w.Header().Set(HeaderDPoPNonce, nonce)
		ctx = context.WithValue(ctx, sessionContextKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeResourceError(w http.ResponseWriter, status int, code, description, nonce string) {
	challenge := `DPoP error="` + code + `"`
	if description != "" {
		challenge += `, error_description="` + description + `"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	if nonce != "" {
		w.Header().Set(HeaderDPoPNonce, nonce)
	}
	writeOAuthError(w, status, code, description)
}

func authCredentials(header string) (string, string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) ValidateAccessToken(ctx context.Context, token string) (*Session, error) {
	session, _, err := s.validateAccessToken(ctx, token)
	return session, err
}

func (s *Server) validateAccessToken(ctx context.Context, token string) (*Session, *tokenClaims, error) {
	now := time.Now()
	claims, err := parseOAuthToken(token, TokenAccess, s.signer, now)
	if err != nil {
		return nil, nil, err
	}
	record, err := s.tokens.GetToken(ctx, claims.JTI)
	if err != nil || record == nil {
		return nil, nil, fmt.Errorf("%w: access token not found", ErrInvalidToken)
	}
	if record.Kind != TokenAccess || record.RevokedAt != nil || !record.ExpiresAt.After(now) {
		return nil, nil, fmt.Errorf("%w: access token is not active", ErrInvalidToken)
	}
	if record.SessionID == "" {
		return nil, nil, fmt.Errorf("%w: access token has no session", ErrInvalidToken)
	}
	session, err := s.sessions.GetSession(ctx, record.SessionID)
	if err != nil || session == nil {
		return nil, nil, fmt.Errorf("%w: session not found", ErrInvalidToken)
	}
	if session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return nil, nil, fmt.Errorf("%w: session is not active", ErrInvalidToken)
	}
	if claims.Sub != session.Account.DID.String() {
		return nil, nil, fmt.Errorf("%w: subject mismatch", ErrInvalidToken)
	}
	if claims.CNF.JKT != session.JKT {
		return nil, nil, fmt.Errorf("%w: binding mismatch", ErrInvalidToken)
	}
	return session, claims, nil
}

func validateCodeChallenge(challenge, method string) error {
	if method != "S256" {
		return fmt.Errorf("%w: code_challenge_method must be S256", ErrInvalidRequest)
	}
	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("%w: invalid code_challenge", ErrInvalidRequest)
	}
	return nil
}

func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for i := 0; i < len(verifier); i++ {
		c := verifier[i]
		unreserved := (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~'
		if !unreserved {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

func normalizeScope(requested, registered string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		requested = ScopeAtproto
	}
	tokens := strings.Fields(requested)
	if len(tokens) == 0 {
		return "", fmt.Errorf("%w: empty scope", ErrInvalidScope)
	}
	foundAtproto := false
	for _, token := range tokens {
		if token == ScopeAtproto {
			foundAtproto = true
		}
	}
	if !foundAtproto {
		return "", fmt.Errorf("%w: missing atproto", ErrInvalidScope)
	}
	if registered != "" {
		allowed := make(map[string]bool)
		for _, token := range strings.Fields(registered) {
			allowed[token] = true
		}
		for _, token := range tokens {
			if !allowed[token] {
				return "", fmt.Errorf("%w: %s is not registered", ErrInvalidScope, token)
			}
		}
	}
	return strings.Join(tokens, " "), nil
}

func authorizeRedirect(redirectURI, code, state, issuer string) (string, error) {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Scheme == "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: invalid redirect_uri", ErrInvalidRedirectURI)
	}
	query := parsed.Query()
	query.Set("code", code)
	if state != "" {
		query.Set("state", state)
	}
	query.Set("iss", issuer)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

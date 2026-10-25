package oauthclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
)

const (
	AccessSkew = 30 * time.Second

	DefaultHTTPTimeout = 30 * time.Second

	ServiceAuthNSID = xrpc.ServiceAuthNSID
)

type Options struct {
	DID string

	Handle string

	RepoDID string

	PDSURL string

	ClientID string

	Scopes []string

	Directory identity.Directory

	HTTPClient *http.Client

	Store SessionStore

	Log *slog.Logger
}

type Session struct {
	mu sync.Mutex

	data SessionData

	key atcrypto.PrivateKeyExportable

	client *http.Client

	store SessionStore

	log *slog.Logger

	did string

	hostURL string
}

var _ pdsclient.TokenSource = (*Session)(nil)

var _ pdsclient.DPoPSource = (*Session)(nil)

func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.Store == nil {
		return nil, errors.New("oauthclient: no oauth session store")
	}
	sessions, err := opts.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, ErrNoSession
	}
	did, err := sessionDID(ctx, opts, sessions)
	if err != nil {
		return nil, err
	}
	session, ok := selectSession(sessions, did)
	if !ok {
		return nil, fmt.Errorf("oauthclient: no oauth session stored for %s", did)
	}
	if session.HostURL == "" {
		return nil, fmt.Errorf("%w: the oauth session names no PDS", ErrInvalidSession)
	}
	if opts.RepoDID != "" && opts.RepoDID != did {
		return nil, fmt.Errorf("%w: the oauth session is for %s, not %s", ErrInvalidSession, did, opts.RepoDID)
	}
	if opts.PDSURL != "" && strings.TrimRight(opts.PDSURL, "/") != strings.TrimRight(session.HostURL, "/") {
		return nil, fmt.Errorf("%w: the oauth session names PDS %s, not %s", ErrInvalidSession, session.HostURL, opts.PDSURL)
	}
	instance, err := New(session, opts)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSession, err)
	}
	_ = instance.EnsureEndpoints(ctx)
	if session.ClientID == "" && strings.TrimSpace(opts.ClientID) != "" {
		_ = instance.persist(ctx)
	}
	if !AccessTokenLive(session.AccessToken) {
		if err := instance.refreshStale(ctx); err != nil {
			return nil, fmt.Errorf("oauthclient: refreshing the oauth session for %s: %w", did, err)
		}
	}
	return instance, nil
}

func (s *Session) EnsureEndpoints(ctx context.Context) error {
	if s == nil {
		return errors.New("oauthclient: no oauth session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.AuthServerTokenEndpoint != "" {
		return nil
	}
	if err := s.ensureEndpointsLocked(ctx); err != nil {
		return err
	}
	return s.persistLocked(ctx)
}

func New(data SessionData, opts Options) (*Session, error) {
	did, err := syntax.ParseDID(data.AccountDID)
	if err != nil {
		return nil, fmt.Errorf("oauthclient: the oauth session account DID: %w", err)
	}
	if data.SessionID == "" {
		data.SessionID = SessionID(did.String(), strings.TrimRight(data.HostURL, "/"))
	}
	if data.HostURL == "" {
		return nil, errors.New("oauthclient: the oauth session names no PDS")
	}
	if data.ClientID == "" {
		data.ClientID = strings.TrimSpace(opts.ClientID)
	}
	host, err := url.Parse(data.HostURL)
	if err != nil || host.Host == "" || (host.Scheme != "http" && host.Scheme != "https") {
		return nil, fmt.Errorf("oauthclient: the oauth session PDS %q is not an absolute http or https URL", data.HostURL)
	}
	key, err := atcrypto.ParsePrivateMultibase(data.DPoPPrivateKeyMultibase)
	if err != nil {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key: %w", err)
	}
	if _, ok := key.(*atcrypto.PrivateKeyP256); !ok {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key type %T is not P-256", key)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Session{
		data:    data,
		key:     key,
		client:  client,
		store:   opts.Store,
		log:     log,
		did:     did.String(),
		hostURL: strings.TrimRight(data.HostURL, "/"),
	}, nil
}

func sessionDID(ctx context.Context, opts Options, sessions []SessionData) (string, error) {
	if opts.DID != "" {
		did, err := syntax.ParseDID(opts.DID)
		if err != nil {
			return "", fmt.Errorf("oauthclient: oauth identity DID: %w", err)
		}
		return did.String(), nil
	}
	if opts.Handle != "" {
		if opts.Directory == nil {
			return "", errors.New("oauthclient: no identity directory resolves the oauth handle")
		}
		atid, err := syntax.ParseAtIdentifier(opts.Handle)
		if err != nil {
			return "", fmt.Errorf("oauthclient: oauth handle %q: %w", opts.Handle, err)
		}
		ident, err := opts.Directory.Lookup(ctx, atid)
		if err != nil {
			return "", fmt.Errorf("oauthclient: resolving the oauth handle %q: %w", opts.Handle, err)
		}
		return ident.DID.String(), nil
	}
	dids := map[string]bool{}
	for _, session := range sessions {
		if session.AccountDID != "" {
			dids[session.AccountDID] = true
		}
	}
	if len(dids) != 1 {
		return "", errors.New("oauthclient: oauth identity mode names no DID or handle")
	}
	for did := range dids {
		return did, nil
	}
	return "", errors.New("oauthclient: oauth identity mode names no DID or handle")
}

func selectSession(sessions []SessionData, did string) (SessionData, bool) {
	prefix := did + "/"
	keys := []string{}
	byKey := map[string]SessionData{}
	for _, session := range sessions {
		key := SessionKey(session.AccountDID, session.SessionID)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
			byKey[key] = session
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return SessionData{}, false
	}
	return byKey[keys[0]], true
}

func (s *Session) DID() string {
	if s == nil {
		return ""
	}
	return s.did
}

func (s *Session) HostURL() string {
	if s == nil {
		return ""
	}
	return s.hostURL
}

func (s *Session) AuthServerURL() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AuthServerURL
}

func (s *Session) Scopes() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if scopes := TokenScopes(s.data.AccessToken); len(scopes) > 0 {
		return scopes
	}
	return ReadableScopes(s.data.Scopes)
}

func TokenScopes(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Scope string `json:"scope"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return ReadableScopes(strings.Fields(claims.Scope))
}

func ReadableScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if !strings.HasPrefix(scope, "ref:") {
			out = append(out, scope)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Session) AccessToken(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("oauthclient: no oauth session")
	}
	s.mu.Lock()
	token := s.data.AccessToken
	s.mu.Unlock()
	if AccessTokenLive(token) {
		return token, nil
	}
	if err := s.refreshStale(ctx); err != nil {
		return "", fmt.Errorf("oauthclient: refreshing the oauth session: %w", err)
	}
	s.mu.Lock()
	token = s.data.AccessToken
	s.mu.Unlock()
	if token == "" {
		return "", errors.New("oauthclient: the oauth session has no access token")
	}
	return token, nil
}

func (s *Session) RefreshAccessToken(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("oauthclient: no oauth session")
	}
	if err := s.refresh(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.AccessToken == "" {
		return "", errors.New("oauthclient: the oauth session has no access token")
	}
	return s.data.AccessToken, nil
}

func (s *Session) SetDPoPNonce(ctx context.Context, host, nonce string) error {
	if s == nil {
		return errors.New("oauthclient: no oauth session")
	}
	if strings.TrimSpace(nonce) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.DPoPHostNonce == nonce {
		return nil
	}
	s.data.DPoPHostNonce = nonce
	return s.persistLocked(ctx)
}

func (s *Session) DPoPProof(ctx context.Context, method, target, accessToken string) (string, error) {
	if s == nil {
		return "", errors.New("oauthclient: no oauth session")
	}
	s.mu.Lock()
	key := s.key
	nonce := s.data.DPoPHostNonce
	s.mu.Unlock()
	return DPoPProof(key, method, target, accessToken, nonce)
}

func (s *Session) HTTPClient(base *http.Client) *http.Client {
	var client http.Client
	if base != nil {
		client = *base
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &httpTransport{session: s, base: transport}
	return &client
}

type httpTransport struct {
	session *Session

	base http.RoundTripper
}

func (t *httpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.session == nil {
		return nil, errors.New("oauthclient: no oauth session")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return t.session.doAuthorized(req.Context(), &http.Client{Transport: base}, req)
}

func (s *Session) ServiceAuth(ctx context.Context, audience, lxm string, expires time.Time) (string, error) {
	if s == nil {
		return "", errors.New("oauthclient: no oauth session")
	}
	if audience == "" {
		return "", errors.New("oauthclient: no service auth audience")
	}
	client := &xrpc.Client{Host: s.hostURL, HTTP: s.HTTPClient(s.client), Limit: ReplyLimit}
	token, err := xrpc.GetServiceAuth(ctx, client, audience, lxm, expires)
	if err != nil {
		return "", fmt.Errorf("oauthclient: service auth: %w", err)
	}
	return token, nil
}

func (s *Session) doAuthorized(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	refreshed := false
	for attempt := range 3 {
		s.mu.Lock()
		accessToken := s.data.AccessToken
		nonce := s.data.DPoPHostNonce
		key := s.key
		s.mu.Unlock()
		proof, err := DPoPProof(key, req.Method, req.URL.String(), accessToken, nonce)
		if err != nil {
			return nil, err
		}
		current := req
		if attempt > 0 {
			current, err = cloneRequest(req)
			if err != nil {
				return nil, err
			}
		}
		current.Header.Set("Authorization", "DPoP "+accessToken)
		current.Header.Set("DPoP", proof)
		resp, err := client.Do(current)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusBadRequest {
			return resp, nil
		}
		header := resp.Header.Get("WWW-Authenticate")
		nextNonce := resp.Header.Get("DPoP-Nonce")
		if nextNonce != "" && (strings.Contains(header, "use_dpop_nonce") || !strings.Contains(header, "invalid_token")) {
			s.mu.Lock()
			s.data.DPoPHostNonce = nextNonce
			err = s.persistLocked(ctx)
			s.mu.Unlock()
			DrainAndClose(resp.Body)
			if err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshed {
			refreshed = true
			DrainAndClose(resp.Body)
			if err := s.refresh(ctx); err != nil {
				return nil, fmt.Errorf("oauthclient: refreshing the oauth session: %w", err)
			}
			continue
		}
		DrainAndClose(resp.Body)
		return nil, fmt.Errorf("oauthclient: %s %s answered HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return nil, errors.New("oauthclient: the oauth session kept asking for a new DPoP nonce")
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	return clone, nil
}

func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, ReplyLimit))
	_ = body.Close()
}

func (s *Session) refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx)
}

func (s *Session) refreshStale(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if AccessTokenLive(s.data.AccessToken) {
		return nil
	}
	return s.refreshLocked(ctx)
}

func (s *Session) refreshLocked(ctx context.Context) error {
	if err := s.refreshOnceLocked(ctx); err != nil {
		return err
	}
	s.log.Info("oauth_session_refreshed", "did", s.did, "session_id", s.data.SessionID)
	return nil
}

func (s *Session) refreshOnceLocked(ctx context.Context) error {
	if s.data.RefreshToken == "" {
		return ErrNoRefreshToken
	}
	if s.data.ClientID == "" {
		return errors.New("oauthclient: the oauth session names no client_id; the token refresh needs it")
	}
	if err := s.ensureEndpointsLocked(ctx); err != nil {
		return err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.data.RefreshToken},
		"client_id":     {s.data.ClientID},
	}
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	var resp *http.Response
	for attempt := range 2 {
		proof, err := DPoPProof(s.key, http.MethodPost, s.data.AuthServerTokenEndpoint, "", s.data.DPoPAuthServerNonce)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.data.AuthServerTokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err = client.Do(req)
		if err != nil {
			return err
		}
		nextNonce := resp.Header.Get("DPoP-Nonce")
		if attempt == 0 && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) && nextNonce != "" {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
			resp.Body.Close()
			if strings.Contains(string(raw), "use_dpop_nonce") {
				s.data.DPoPAuthServerNonce = nextNonce
				continue
			}
			return fmt.Errorf("oauthclient: the token refresh answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		break
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) && strings.Contains(string(raw), "invalid_grant") {
			return fmt.Errorf("%w: %s", ErrSessionExpired, strings.TrimSpace(string(raw)))
		}
		return fmt.Errorf("oauthclient: the token refresh answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("oauthclient: decoding the token refresh: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return errors.New("oauthclient: the token refresh returned no access or refresh token")
	}
	s.data.AccessToken = out.AccessToken
	s.data.RefreshToken = out.RefreshToken
	if granted := ReadableScopes(strings.Fields(out.Scope)); len(granted) > 0 {
		s.data.Scopes = granted
	}
	if nextNonce := resp.Header.Get("DPoP-Nonce"); nextNonce != "" {
		s.data.DPoPAuthServerNonce = nextNonce
	}
	return s.persistLocked(ctx)
}

func (s *Session) persist(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked(ctx)
}

func (s *Session) persistLocked(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	return s.store.Save(ctx, s.data)
}

func (s *Session) ensureEndpointsLocked(ctx context.Context) error {
	if s.data.AuthServerTokenEndpoint != "" {
		return nil
	}
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	authServer, tokenEndpoint, revokeEndpoint, err := DiscoverAuthServer(ctx, client, s.hostURL)
	if err != nil {
		return err
	}
	s.data.AuthServerURL = authServer
	s.data.AuthServerTokenEndpoint = tokenEndpoint
	if revokeEndpoint != "" {
		s.data.AuthServerRevocationEndpoint = revokeEndpoint
	}
	return nil
}

func AccessTokenLive(token string) bool {
	if token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(AccessSkew).Before(time.Unix(claims.ExpiresAt, 0))
}

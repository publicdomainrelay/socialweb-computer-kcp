package oauthclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
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

	Refresher TokenRefresher

	Log *slog.Logger
}

type Session struct {
	mu sync.Mutex

	data SessionData

	key atcrypto.PrivateKeyExportable

	client *http.Client

	store SessionStore

	refresher TokenRefresher

	log *slog.Logger

	did string

	hostURL string

	scopeAbsentLogged bool

	refreshToken atomic.Pointer[string]
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
	key, err := parseDPoPKey(data.DPoPPrivateKeyMultibase)
	if err != nil {
		return nil, err
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	session := &Session{
		data:      data,
		key:       key,
		client:    client,
		store:     opts.Store,
		refresher: opts.Refresher,
		log:       log,
		did:       did.String(),
		hostURL:   strings.TrimRight(data.HostURL, "/"),
	}
	session.refreshToken.Store(&data.RefreshToken)
	return session, nil
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
	return append([]string(nil), s.data.Scopes...)
}

func GrantedScopes(scope string) ([]string, error) {
	granted := strings.Fields(scope)
	if atprotooauth.KnownGrant(granted) && !slices.Contains(granted, atprotooauth.ScopeAtproto) {
		return nil, fmt.Errorf("%w (granted %q)", ErrScopeRejected, scope)
	}
	return granted, nil
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
	s.mu.Lock()
	rejected := s.data.AccessToken
	s.mu.Unlock()
	if err := s.refreshRejected(ctx, rejected); err != nil {
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
			if err := s.refreshRejected(ctx, accessToken); err != nil {
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

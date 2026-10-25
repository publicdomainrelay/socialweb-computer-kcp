package oauthclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

const RefreshTimeout = 30 * time.Second

const (
	refreshOutcomeRefreshed = "refreshed"

	refreshOutcomeFromStore = "adopted_from_store"

	refreshOutcomeRecovered = "recovered_from_store"

	refreshOutcomeRecoveredRefreshed = "recovered_from_store_refreshed"

	refreshOutcomeUnsaved = "refreshed_unsaved"

	refreshOutcomeExpired = "expired"

	refreshOutcomeError = "error"
)

type refreshFlight struct {
	from string

	sessions []*Session

	done chan struct{}

	err error
}

type refreshResult struct {
	data SessionData

	outcome string

	save bool
}

var refreshFlights = struct {
	mu sync.Mutex

	byToken map[string]*refreshFlight
}{byToken: map[string]*refreshFlight{}}

func refreshFlightKey(refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:])
}

func (s *Session) refreshStale(ctx context.Context) error {
	s.mu.Lock()
	current := s.data
	s.mu.Unlock()
	if AccessTokenLive(current.AccessToken) {
		return nil
	}
	return s.refreshFrom(ctx, current)
}

func (s *Session) refreshRejected(ctx context.Context, rejected string) error {
	s.mu.Lock()
	current := s.data
	s.mu.Unlock()
	if current.AccessToken != rejected {
		return nil
	}
	return s.refreshFrom(ctx, current)
}

func (s *Session) refreshFrom(ctx context.Context, current SessionData) error {
	if current.RefreshToken == "" {
		return ErrNoRefreshToken
	}
	if current.ClientID == "" {
		return errors.New("oauthclient: the oauth session names no client_id; the token refresh needs it")
	}
	flight := s.joinRefresh(ctx, current)
	if flight == nil {
		return nil
	}
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return fmt.Errorf("oauthclient: waiting for the token refresh: %w", context.Cause(ctx))
	}
}

func (s *Session) joinRefresh(ctx context.Context, current SessionData) *refreshFlight {
	key := refreshFlightKey(current.RefreshToken)
	refreshFlights.mu.Lock()
	defer refreshFlights.mu.Unlock()
	if flight, ok := refreshFlights.byToken[key]; ok {
		if !slices.Contains(flight.sessions, s) {
			flight.sessions = append(flight.sessions, s)
		}
		return flight
	}
	if s.currentRefreshToken() != current.RefreshToken {
		return nil
	}
	flight := &refreshFlight{from: current.RefreshToken, sessions: []*Session{s}, done: make(chan struct{})}
	refreshFlights.byToken[key] = flight
	go s.runRefresh(context.WithoutCancel(ctx), key, flight, current)
	return flight
}

func (s *Session) runRefresh(base context.Context, key string, flight *refreshFlight, current SessionData) {
	defer close(flight.done)
	result, err := s.rotate(base, current)
	if err == nil {
		if serr := s.commitRefresh(base, flight.from, result.data, result.save); serr != nil {
			result.outcome = refreshOutcomeUnsaved
			err = fmt.Errorf("oauthclient: saving the refreshed oauth session: %w", serr)
		}
	}
	refreshFlights.mu.Lock()
	delete(refreshFlights.byToken, key)
	sessions := flight.sessions
	refreshFlights.mu.Unlock()
	if result.data.RefreshToken != "" {
		for _, other := range sessions {
			if other == s {
				continue
			}
			if cerr := other.commitRefresh(base, flight.from, result.data, result.save); cerr != nil {
				other.log.Warn("oauth_session_refresh_commit_failed", "did", other.did, "session_id", current.SessionID)
			}
		}
	}
	flight.err = err
	switch {
	case err == nil:
		s.log.Info("oauth_session_refreshed", "did", s.did, "session_id", current.SessionID, "outcome", result.outcome, "waiters", len(sessions))
	case result.outcome == refreshOutcomeUnsaved:
		s.log.Warn("oauth_session_refresh_failed", "did", s.did, "session_id", current.SessionID, "outcome", result.outcome, "waiters", len(sessions))
	case errors.Is(err, ErrSessionExpired):
		s.log.Warn("oauth_session_refresh_failed", "did", s.did, "session_id", current.SessionID, "outcome", refreshOutcomeExpired, "waiters", len(sessions))
	default:
		s.log.Warn("oauth_session_refresh_failed", "did", s.did, "session_id", current.SessionID, "outcome", refreshOutcomeError, "waiters", len(sessions))
	}
}

func (s *Session) rotate(base context.Context, current SessionData) (refreshResult, error) {
	data := current
	if stored, ok := s.storedSession(base, current); ok && newerTokenSet(stored, current) {
		if AccessTokenLive(stored.AccessToken) {
			return refreshResult{data: stored, outcome: refreshOutcomeFromStore}, nil
		}
		data = stored
	}
	next, err := s.requestTokens(base, data)
	if err == nil {
		return refreshResult{data: next, outcome: refreshOutcomeRefreshed, save: true}, nil
	}
	if !errors.Is(err, ErrSessionExpired) {
		return refreshResult{}, err
	}
	stored, ok := s.storedSession(base, current)
	if !ok || stored.RefreshToken == data.RefreshToken {
		return refreshResult{}, err
	}
	if AccessTokenLive(stored.AccessToken) {
		return refreshResult{data: stored, outcome: refreshOutcomeRecovered}, nil
	}
	next, err = s.requestTokens(base, stored)
	if err != nil {
		return refreshResult{}, err
	}
	return refreshResult{data: next, outcome: refreshOutcomeRecoveredRefreshed, save: true}, nil
}

func (s *Session) commitRefresh(base context.Context, from string, data SessionData, save bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.RefreshToken != from {
		return nil
	}
	next := data
	next.AccountDID, next.SessionID = s.data.AccountDID, s.data.SessionID
	if next.ClientID == "" {
		next.ClientID = s.data.ClientID
	}
	key := s.key
	if next.DPoPPrivateKeyMultibase == s.data.DPoPPrivateKeyMultibase {
		if s.data.DPoPHostNonce != "" {
			next.DPoPHostNonce = s.data.DPoPHostNonce
		}
	} else {
		parsed, err := parseDPoPKey(next.DPoPPrivateKeyMultibase)
		if err != nil {
			return err
		}
		key = parsed
	}
	var err error
	if save && s.store != nil {
		ctx, cancel := context.WithTimeout(base, RefreshTimeout)
		err = s.store.Save(ctx, next)
		cancel()
	}
	s.data, s.key = next, key
	s.refreshToken.Store(&next.RefreshToken)
	return err
}

func (s *Session) currentRefreshToken() string {
	if token := s.refreshToken.Load(); token != nil {
		return *token
	}
	return ""
}

func (s *Session) storedSession(base context.Context, current SessionData) (SessionData, bool) {
	if s.store == nil {
		return SessionData{}, false
	}
	ctx, cancel := context.WithTimeout(base, RefreshTimeout)
	defer cancel()
	sessions, err := s.store.List(ctx)
	if err != nil {
		s.log.Warn("oauth_session_store_read_failed", "did", s.did, "session_id", current.SessionID)
		return SessionData{}, false
	}
	for _, stored := range sessions {
		if stored.AccountDID != s.did || strings.TrimRight(stored.HostURL, "/") != s.hostURL {
			continue
		}
		if stored.SessionID != "" && stored.SessionID != current.SessionID {
			continue
		}
		if stored.RefreshToken == "" || stored.DPoPPrivateKeyMultibase == "" {
			continue
		}
		stored.SessionID = current.SessionID
		if stored.ClientID == "" {
			stored.ClientID = current.ClientID
		}
		if stored.AuthServerTokenEndpoint == "" {
			stored.AuthServerURL = current.AuthServerURL
			stored.AuthServerTokenEndpoint = current.AuthServerTokenEndpoint
			stored.AuthServerRevocationEndpoint = current.AuthServerRevocationEndpoint
		}
		return stored, true
	}
	return SessionData{}, false
}

func newerTokenSet(stored, current SessionData) bool {
	if stored.RefreshToken == "" || stored.RefreshToken == current.RefreshToken {
		return false
	}
	storedExpiry, storedOK := accessTokenExpiry(stored.AccessToken)
	currentExpiry, currentOK := accessTokenExpiry(current.AccessToken)
	return !storedOK || !currentOK || !storedExpiry.Before(currentExpiry)
}

func (s *Session) requestTokens(base context.Context, data SessionData) (SessionData, error) {
	if data.AuthServerTokenEndpoint == "" {
		ctx, cancel := context.WithTimeout(base, RefreshTimeout)
		authServer, tokenEndpoint, revokeEndpoint, err := DiscoverAuthServer(ctx, s.httpClient(), s.hostURL)
		cancel()
		if err != nil {
			return SessionData{}, err
		}
		data.AuthServerURL = authServer
		data.AuthServerTokenEndpoint = tokenEndpoint
		if revokeEndpoint != "" {
			data.AuthServerRevocationEndpoint = revokeEndpoint
		}
	}
	if refresher := s.tokenRefresher(); refresher != nil {
		next, err := s.refreshVia(base, refresher, data)
		if !errors.Is(err, ErrRefreshPassThrough) {
			return next, err
		}
	}
	key, err := parseDPoPKey(data.DPoPPrivateKeyMultibase)
	if err != nil {
		return SessionData{}, err
	}
	ctx, cancel := context.WithTimeout(base, RefreshTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {data.RefreshToken},
		"client_id":     {data.ClientID},
	}
	client := s.httpClient()
	var resp *http.Response
	for attempt := range 2 {
		proof, err := DPoPProof(key, http.MethodPost, data.AuthServerTokenEndpoint, "", data.DPoPAuthServerNonce)
		if err != nil {
			return SessionData{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, data.AuthServerTokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return SessionData{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err = client.Do(req)
		if err != nil {
			return SessionData{}, err
		}
		nextNonce := resp.Header.Get("DPoP-Nonce")
		if attempt == 0 && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) && nextNonce != "" {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
			resp.Body.Close()
			if strings.Contains(string(raw), "use_dpop_nonce") {
				data.DPoPAuthServerNonce = nextNonce
				continue
			}
			return SessionData{}, tokenRefreshError(resp.StatusCode, raw)
		}
		break
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
	if err != nil {
		return SessionData{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SessionData{}, tokenRefreshError(resp.StatusCode, raw)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: decoding the token refresh: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return SessionData{}, errors.New("oauthclient: the token refresh returned no access or refresh token")
	}
	if out.Scope == "" {
		s.mu.Lock()
		first := !s.scopeAbsentLogged
		s.scopeAbsentLogged = true
		s.mu.Unlock()
		if first {
			s.log.Info("oauth_refresh_scope_absent", "did", s.did, "session_id", data.SessionID, "kept", strings.Join(data.Scopes, " "))
		}
	} else {
		granted, err := GrantedScopes(out.Scope)
		if err != nil {
			return SessionData{}, fmt.Errorf("%w: the token refresh: %w", ErrSessionExpired, err)
		}
		data.Scopes = granted
	}
	data.AccessToken = out.AccessToken
	data.RefreshToken = out.RefreshToken
	if nextNonce := resp.Header.Get("DPoP-Nonce"); nextNonce != "" {
		data.DPoPAuthServerNonce = nextNonce
	}
	return data, nil
}

func tokenRefreshError(status int, raw []byte) error {
	body := strings.TrimSpace(string(raw))
	if (status == http.StatusBadRequest || status == http.StatusUnauthorized) && strings.Contains(body, "invalid_grant") {
		return fmt.Errorf("%w: %s", ErrSessionExpired, body)
	}
	return fmt.Errorf("oauthclient: the token refresh answered HTTP %d: %s", status, body)
}

func (s *Session) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

func parseDPoPKey(multibase string) (atcrypto.PrivateKeyExportable, error) {
	key, err := atcrypto.ParsePrivateMultibase(multibase)
	if err != nil {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key: %w", err)
	}
	if _, ok := key.(*atcrypto.PrivateKeyP256); !ok {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key type %T is not P-256", key)
	}
	return key, nil
}

func AccessTokenLive(token string) bool {
	expiry, ok := accessTokenExpiry(token)
	return ok && time.Now().Add(AccessSkew).Before(expiry)
}

func accessTokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.ExpiresAt == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.ExpiresAt, 0), true
}

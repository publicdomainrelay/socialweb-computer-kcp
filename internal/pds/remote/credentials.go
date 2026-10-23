package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

var ErrSessionExpired = errors.New("remote: the RemotePDS session expired and its refresh token is refused")

var (
	accessKeys = []string{"accessJwt", "accessToken", "access_token", "jwt", "token", "value"}

	refreshKeys = []string{"refreshJwt", "refreshToken", "refresh_token"}
)

const refreshTimeout = 30 * time.Second

type SecretWriter func(ctx context.Context, ref commonv1.SecretRef, data map[string]any) error

type SessionConfig struct {
	Auth pdsv1alpha1.RemotePDSAuth

	URL string

	TLS *tls.Config

	Read Secrets

	Write SecretWriter
}

type Session struct {
	cfg SessionConfig

	access *commonv1.SecretRef

	refresh *commonv1.SecretRef

	tokens *lease.Source[string]

	auth xrpc.Auth

	oauthMode bool

	mu sync.Mutex

	oauth *oauthclient.Session

	rejected string

	dead string
}

var (
	_ pdsclient.TokenSource     = (*Session)(nil)
	_ pdsclient.TokenRefresher  = (*Session)(nil)
	_ pdsclient.DPoPSource      = oauthDPoP{}
	_ pdsclient.DPoPNonceSetter = oauthDPoP{}
	_ oauthclient.SessionStore  = sessionStore{}
)

func NewSession(ctx context.Context, cfg SessionConfig) (*Session, error) {
	access, refresh, keyRef := usable(cfg.Auth.AccessTokenRef), usable(cfg.Auth.RefreshTokenRef), usable(cfg.Auth.DPoPKeyRef)
	if access == nil {
		access = refresh
	}
	if access == nil && keyRef == nil {
		return nil, nil
	}
	if access == nil {
		return nil, errors.New("remote: the RemotePDS auth names a dpop key but no session token ref")
	}
	if cfg.Read == nil {
		return nil, errors.New("remote: the RemotePDS auth names refs but no secret reader is configured")
	}
	s := &Session{cfg: cfg, access: access, refresh: refresh}
	s.tokens = lease.New(s.fetch)
	data, err := cfg.Read(ctx, *access)
	if err != nil {
		return nil, fmt.Errorf("remote: reading the session ref %s: %w", refName(*access), err)
	}
	var dpop pdsclient.DPoPSource
	if _, ok := oauthclient.SessionFromSecret(data); ok {
		s.oauthMode = true
		dpop = oauthDPoP{s: s}
	} else if keyRef != nil {
		keyData, err := cfg.Read(ctx, *keyRef)
		if err != nil {
			return nil, fmt.Errorf("remote: resolving the dpop key ref %s: %w", refName(*keyRef), err)
		}
		if dpop, err = pdsclient.DPoPSourceFromData(keyData); err != nil {
			return nil, err
		}
	}
	s.auth = pdsclient.TokenAuth(s, dpop)
	return s, nil
}

func (s *Session) Auth() xrpc.Auth {
	if s == nil {
		return nil
	}
	return s.auth
}

func (s *Session) Check(ctx context.Context) error {
	_, err := s.AccessToken(ctx)
	return err
}

func (s *Session) AccessToken(ctx context.Context) (string, error) {
	return s.tokens.Get(ctx)
}

func (s *Session) RefreshAccessToken(ctx context.Context) (string, error) {
	if held, _, ok := s.tokens.Peek(); ok {
		s.mu.Lock()
		s.rejected = held
		s.mu.Unlock()
	}
	s.tokens.Invalidate()
	return s.tokens.Get(ctx)
}

func (s *Session) fetch(ctx context.Context) (string, time.Time, error) {
	s.mu.Lock()
	rejected := s.rejected
	s.mu.Unlock()
	var token string
	var err error
	if s.oauthMode {
		token, err = s.fetchOAuth(ctx, rejected)
	} else {
		token, err = s.fetchBearer(ctx, rejected)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	s.rejected = ""
	s.mu.Unlock()
	expires, ok := pdsclient.TokenExpiry(token)
	if !ok {
		return token, time.Time{}, nil
	}
	return token, expires.Add(-pdsclient.TokenExpirySkew), nil
}

func (s *Session) fetchBearer(ctx context.Context, rejected string) (string, error) {
	data, err := s.cfg.Read(ctx, *s.access)
	if err != nil {
		return "", fmt.Errorf("remote: resolving the token ref %s: %w", refName(*s.access), err)
	}
	access := lookup(data, s.access.Key, accessKeys)
	if access != "" && access != rejected && pdsclient.TokenLive(access) {
		return access, nil
	}
	if s.refresh == nil {
		return "", fmt.Errorf("%w: the RemotePDS session holds no live token and names no refreshTokenRef", pdsclient.ErrAuthenticationRequired)
	}
	refreshData := data
	if !sameRef(*s.access, *s.refresh) {
		if refreshData, err = s.cfg.Read(ctx, *s.refresh); err != nil {
			return "", fmt.Errorf("remote: resolving the refresh token ref %s: %w", refName(*s.refresh), err)
		}
	}
	refresh := lookup(refreshData, s.refresh.Key, refreshKeys)
	if refresh == "" {
		return "", fmt.Errorf("%w: the RemotePDS session holds no live token and no refresh token", pdsclient.ErrAuthenticationRequired)
	}
	if s.isDead(refresh) {
		return "", ErrSessionExpired
	}
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: s.cfg.URL, TLS: s.cfg.TLS, Timeout: refreshTimeout})
	if err != nil {
		return "", err
	}
	session, err := client.RefreshSession(ctx, refresh)
	if err != nil {
		if refused(err) {
			s.markDead(refresh)
			return "", fmt.Errorf("%w: %v", ErrSessionExpired, err)
		}
		return "", fmt.Errorf("remote: refreshing the RemotePDS session: %w", err)
	}
	if s.cfg.Write == nil {
		return "", errors.New("remote: the refreshed RemotePDS session has no secret writer")
	}
	if err := s.cfg.Write(ctx, *s.refresh, withTokens(refreshData, s.refresh.Key, refreshKeys[0], session)); err != nil {
		return "", fmt.Errorf("remote: storing the refreshed RemotePDS session: %w", err)
	}
	if !sameRef(*s.access, *s.refresh) {
		if err := s.cfg.Write(ctx, *s.access, withTokens(data, s.access.Key, accessKeys[0], session)); err != nil {
			return "", fmt.Errorf("remote: storing the refreshed RemotePDS access token: %w", err)
		}
	}
	return session.AccessJWT, nil
}

func (s *Session) fetchOAuth(ctx context.Context, rejected string) (string, error) {
	data, err := s.cfg.Read(ctx, *s.access)
	if err != nil {
		return "", fmt.Errorf("remote: resolving the session ref %s: %w", refName(*s.access), err)
	}
	stored, _ := oauthclient.SessionFromSecret(data)
	if stored.RefreshToken != "" && s.isDead(stored.RefreshToken) {
		return "", ErrSessionExpired
	}
	session, err := s.oauthSession(ctx)
	var token string
	if err == nil {
		if rejected != "" {
			token, err = session.RefreshAccessToken(ctx)
		} else {
			token, err = session.AccessToken(ctx)
		}
	}
	if err != nil {
		if errors.Is(err, oauthclient.ErrSessionExpired) || errors.Is(err, oauthclient.ErrNoRefreshToken) {
			s.markDead(stored.RefreshToken)
			s.mu.Lock()
			s.oauth = nil
			s.mu.Unlock()
			return "", fmt.Errorf("%w: %v", ErrSessionExpired, err)
		}
		return "", err
	}
	return token, nil
}

func (s *Session) oauthSession(ctx context.Context) (*oauthclient.Session, error) {
	s.mu.Lock()
	held := s.oauth
	s.mu.Unlock()
	if held != nil {
		return held, nil
	}
	opened, err := oauthclient.Open(ctx, oauthclient.Options{HTTPClient: &http.Client{Timeout: refreshTimeout},
		Store: sessionStore{s: s}})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.oauth = opened
	s.mu.Unlock()
	return opened, nil
}

func (s *Session) isDead(refresh string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return refresh != "" && s.dead == refresh
}

func (s *Session) markDead(refresh string) {
	s.mu.Lock()
	s.dead = refresh
	s.mu.Unlock()
}

type oauthDPoP struct {
	s *Session
}

func (d oauthDPoP) DPoPProof(ctx context.Context, method, url, accessToken string) (string, error) {
	session, err := d.s.oauthSession(ctx)
	if err != nil {
		return "", err
	}
	return session.DPoPProof(ctx, method, url, accessToken)
}

func (d oauthDPoP) SetDPoPNonce(ctx context.Context, host, nonce string) error {
	session, err := d.s.oauthSession(ctx)
	if err != nil {
		return err
	}
	return session.SetDPoPNonce(ctx, host, nonce)
}

type sessionStore struct {
	s *Session
}

func (st sessionStore) List(ctx context.Context) ([]oauthclient.SessionData, error) {
	data, err := st.s.cfg.Read(ctx, *st.s.access)
	if err != nil {
		return nil, err
	}
	session, ok := oauthclient.SessionFromSecret(data)
	if !ok {
		return nil, nil
	}
	return []oauthclient.SessionData{session}, nil
}

func (st sessionStore) Save(ctx context.Context, session oauthclient.SessionData) error {
	if st.s.cfg.Write == nil {
		return errors.New("remote: the RemotePDS oauth session has no secret writer")
	}
	data := map[string]any{}
	if held, err := st.s.cfg.Read(ctx, *st.s.access); err == nil {
		maps.Copy(data, held)
	}
	maps.Copy(data, oauthclient.SessionSecret(session))
	return st.s.cfg.Write(ctx, *st.s.access, data)
}

func (sessionStore) Delete(context.Context, string, string) error {
	return nil
}

func refused(err error) bool {
	var failure *pdsclient.XRPCError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "ExpiredToken", "InvalidToken", "AuthenticationRequired", "AccountTakedown":
		return true
	}
	return failure.StatusCode == http.StatusUnauthorized
}

func withTokens(data map[string]any, key, primary string, session pdsclient.Session) map[string]any {
	out := maps.Clone(data)
	if out == nil {
		out = map[string]any{}
	}
	out["accessJwt"] = session.AccessJWT
	out["refreshJwt"] = session.RefreshJWT
	out["access_token"] = session.AccessJWT
	out["refresh_token"] = session.RefreshJWT
	if key = strings.TrimSpace(key); key != "" {
		if primary == refreshKeys[0] {
			out[key] = session.RefreshJWT
		} else {
			out[key] = session.AccessJWT
		}
	}
	if raw := strs.Lookup(data, "session"); raw != "" {
		var stored oauthclient.SessionData
		if json.Unmarshal([]byte(raw), &stored) == nil {
			stored.AccessToken = session.AccessJWT
			stored.RefreshToken = session.RefreshJWT
			if encoded, err := json.Marshal(stored); err == nil {
				out["session"] = string(encoded)
			}
		}
	}
	return out
}

func lookup(data map[string]any, key string, keys []string) string {
	if key = strings.TrimSpace(key); key != "" {
		keys = []string{key}
	}
	return strings.TrimSpace(strs.Lookup(data, keys...))
}

func sameRef(a, b commonv1.SecretRef) bool {
	vault := func(r commonv1.SecretRef) string {
		if r.Vault == nil {
			return ""
		}
		return r.Vault.String()
	}
	return vault(a) == vault(b) && refName(a) == refName(b)
}

func usable(ref *commonv1.SecretRef) *commonv1.SecretRef {
	if ref == nil || strings.Trim(strings.TrimSpace(ref.Path), "/") == "" {
		return nil
	}
	return ref
}

func refName(ref commonv1.SecretRef) string {
	return strings.Trim(strings.TrimSpace(ref.Mount)+"/"+strings.TrimSpace(ref.Path), "/")
}

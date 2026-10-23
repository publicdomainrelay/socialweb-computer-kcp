package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const callTimeout = 30 * time.Second

var (
	accessKeys = []string{"accessJwt", "accessToken", "access_token", "token", "value"}

	refreshKeys = []string{"refreshJwt", "refreshToken", "refresh_token"}
)

type Store interface {
	Read(ctx context.Context) (map[string]any, error)

	Write(ctx context.Context, data map[string]any) error
}

type Config struct {
	DID string

	PDSURL string

	ClientID string

	Scopes []string

	Store Store

	HTTP *http.Client
}

type Authenticator struct {
	did string

	auth xrpc.Auth
}

var _ pds.Authenticator = (*Authenticator)(nil)

func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if cfg.Store == nil {
		return nil, errors.New("identity: the credentials have no session store")
	}
	data, err := cfg.Store.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: reading the session of %s: %w", cfg.DID, err)
	}
	if _, ok := oauthclient.SessionFromSecret(data); ok {
		session, err := oauthclient.Open(ctx, oauthclient.Options{RepoDID: cfg.DID, PDSURL: cfg.PDSURL, ClientID: cfg.ClientID,
			Scopes: cfg.Scopes, HTTPClient: cfg.HTTP, Store: sessionStore{store: cfg.Store}})
		if err != nil {
			return nil, err
		}
		return &Authenticator{did: session.DID(), auth: pdsclient.TokenAuth(session, session)}, nil
	}
	if strs.Lookup(data, accessKeys...) == "" && strs.Lookup(data, refreshKeys...) == "" {
		return nil, fmt.Errorf("%w: the session of %s holds no token", pdsclient.ErrAuthenticationRequired, cfg.DID)
	}
	tokens := &bearerTokens{store: cfg.Store, pdsURL: cfg.PDSURL, http: cfg.HTTP}
	return &Authenticator{did: cfg.DID, auth: pdsclient.TokenAuth(tokens, nil)}, nil
}

func (a *Authenticator) DID() string { return a.did }

func (a *Authenticator) Apply(req *http.Request) error { return a.auth.Apply(req) }

func (a *Authenticator) Invalidate() { a.auth.Invalidate() }

func (a *Authenticator) Challenge(resp *http.Response) pds.Verdict {
	if challenger, ok := a.auth.(xrpc.Challenger); ok {
		switch challenger.Challenge(resp) {
		case xrpc.RetryNonce:
			return pds.VerdictRetryNonce
		case xrpc.RetryAuth:
			return pds.VerdictRetryAuth
		}
		return pds.VerdictDone
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return pds.VerdictRetryAuth
	}
	return pds.VerdictDone
}

type sessionStore struct {
	store Store
}

var _ oauthclient.SessionStore = sessionStore{}

func (s sessionStore) List(ctx context.Context) ([]oauthclient.SessionData, error) {
	data, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	session, ok := oauthclient.SessionFromSecret(data)
	if !ok {
		return nil, nil
	}
	return []oauthclient.SessionData{session}, nil
}

func (s sessionStore) Save(ctx context.Context, session oauthclient.SessionData) error {
	if session.AccountDID == "" {
		return errors.New("identity: the oauth session names no account DID")
	}
	data := map[string]any{}
	if held, err := s.store.Read(ctx); err == nil {
		maps.Copy(data, held)
	}
	maps.Copy(data, oauthclient.SessionSecret(session))
	return s.store.Write(ctx, data)
}

func (s sessionStore) Delete(context.Context, string, string) error {
	return nil
}

type bearerTokens struct {
	store Store

	pdsURL string

	http *http.Client

	mu sync.Mutex

	access string
}

var (
	_ pdsclient.TokenSource    = (*bearerTokens)(nil)
	_ pdsclient.TokenRefresher = (*bearerTokens)(nil)
)

func (t *bearerTokens) AccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if pdsclient.TokenLive(t.access) {
		return t.access, nil
	}
	return t.renewLocked(ctx, false)
}

func (t *bearerTokens) RefreshAccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.renewLocked(ctx, true)
}

func (t *bearerTokens) renewLocked(ctx context.Context, rejected bool) (string, error) {
	data, err := t.store.Read(ctx)
	if err != nil {
		return "", err
	}
	access := strs.Lookup(data, accessKeys...)
	refresh := strs.Lookup(data, refreshKeys...)
	if !rejected && pdsclient.TokenLive(access) {
		t.access = access
		return access, nil
	}
	if refresh == "" || strings.TrimSpace(t.pdsURL) == "" {
		if access == "" {
			return "", fmt.Errorf("%w: the session holds no live token and no refresh token", pdsclient.ErrAuthenticationRequired)
		}
		t.access = access
		return access, nil
	}
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: t.pdsURL, HTTPClient: t.http, Timeout: callTimeout})
	if err != nil {
		return "", err
	}
	session, err := client.RefreshSession(ctx, refresh)
	if err != nil {
		return "", fmt.Errorf("identity: refreshing the session: %w", err)
	}
	t.access = session.AccessJWT
	updated := maps.Clone(data)
	updated["accessJwt"] = session.AccessJWT
	updated["refreshJwt"] = session.RefreshJWT
	updated["access_token"] = session.AccessJWT
	updated["refresh_token"] = session.RefreshJWT
	if raw := strs.Lookup(data, "session"); raw != "" {
		var stored oauthclient.SessionData
		if json.Unmarshal([]byte(raw), &stored) == nil {
			stored.AccessToken = session.AccessJWT
			stored.RefreshToken = session.RefreshJWT
			if encoded, err := json.Marshal(stored); err == nil {
				updated["session"] = string(encoded)
			}
		}
	}
	if err := t.store.Write(ctx, updated); err != nil {
		return "", fmt.Errorf("identity: storing the refreshed session: %w", err)
	}
	return t.access, nil
}

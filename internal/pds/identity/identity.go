package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

var ErrNotHeld = errors.New("identity: no account session is held")

type Store interface {
	Read(ctx context.Context) (map[string]any, error)

	Write(ctx context.Context, data map[string]any) error
}

type Authenticator struct {
	mu sync.Mutex

	did string

	session *oauthclient.Session

	bearer *Bearer

	proxy *Proxy

	auth xrpc.Auth
}

type Proxy struct {
	PdsURL string

	HTTP *http.Client

	Prover xrpc.DPoPProver
}

var (
	_ pds.Authenticator = (*Authenticator)(nil)
	_ pds.ServiceAuth   = (*Authenticator)(nil)
)

func NewOAuth() *Authenticator {
	a := &Authenticator{}
	source := oauthSource{a: a}
	a.auth = pdsclient.TokenAuth(source, source)
	return a
}

func NewProxied() *Authenticator {
	a := &Authenticator{}
	a.auth = &xrpc.DPoP{Prover: proxySource{a: a}}
	return a
}

func NewBearer() *Authenticator {
	a := &Authenticator{}
	a.auth = pdsclient.TokenAuth(bearerSource{a: a}, nil)
	return a
}

func (a *Authenticator) Hold(session *oauthclient.Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session, a.bearer, a.proxy, a.did = session, nil, nil, session.DID()
}

func (a *Authenticator) HoldBearer(did string, bearer *Bearer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session, a.bearer, a.proxy, a.did = nil, bearer, nil, did
}

func (a *Authenticator) HoldProxy(did string, proxy *Proxy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session, a.bearer, a.proxy, a.did = nil, nil, proxy, did
}

func (a *Authenticator) Drop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session, a.bearer, a.proxy = nil, nil, nil
}

func (a *Authenticator) Proxied() *Proxy {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxy
}

func (a *Authenticator) Session() *oauthclient.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
}

func (a *Authenticator) Bearer() *Bearer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bearer
}

func (a *Authenticator) DID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.did
}

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

func (a *Authenticator) ServiceAuth(ctx context.Context, did, audience, lxm string, expires time.Time) (string, error) {
	a.mu.Lock()
	held, session, bearer, proxy := a.did, a.session, a.bearer, a.proxy
	a.mu.Unlock()
	if did != held {
		return "", fmt.Errorf("identity: the held session signs for %q, not %s", held, did)
	}
	switch {
	case session != nil:
		return session.ServiceAuth(ctx, audience, lxm, expires)
	case bearer != nil:
		client := &xrpc.Client{Host: bearer.pdsURL, HTTP: bearer.http, Auth: a.auth}
		return xrpc.GetServiceAuth(ctx, client, audience, lxm, expires)
	case proxy != nil:
		client := &xrpc.Client{Host: strings.TrimRight(proxy.PdsURL, "/"), HTTP: proxy.HTTP, Auth: a.auth}
		return xrpc.GetServiceAuth(ctx, client, audience, lxm, expires)
	}
	return "", fmt.Errorf("%w for %s", ErrNotHeld, did)
}

func (a *Authenticator) heldSession() (*oauthclient.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		return nil, fmt.Errorf("%w: %w", pdsclient.ErrAuthenticationRequired, ErrNotHeld)
	}
	return a.session, nil
}

func (a *Authenticator) heldBearer() (*Bearer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bearer == nil {
		return nil, fmt.Errorf("%w: %w", pdsclient.ErrAuthenticationRequired, ErrNotHeld)
	}
	return a.bearer, nil
}

type oauthSource struct {
	a *Authenticator
}

var (
	_ pdsclient.TokenSource     = oauthSource{}
	_ pdsclient.TokenRefresher  = oauthSource{}
	_ pdsclient.DPoPSource      = oauthSource{}
	_ pdsclient.DPoPNonceSetter = oauthSource{}
)

func (s oauthSource) AccessToken(ctx context.Context) (string, error) {
	session, err := s.a.heldSession()
	if err != nil {
		return "", err
	}
	return session.AccessToken(ctx)
}

func (s oauthSource) RefreshAccessToken(ctx context.Context) (string, error) {
	session, err := s.a.heldSession()
	if err != nil {
		return "", err
	}
	return session.RefreshAccessToken(ctx)
}

func (s oauthSource) DPoPProof(ctx context.Context, method, target, accessToken string) (string, error) {
	session, err := s.a.heldSession()
	if err != nil {
		return "", err
	}
	return session.DPoPProof(ctx, method, target, accessToken)
}

func (s oauthSource) SetDPoPNonce(ctx context.Context, host, nonce string) error {
	session, err := s.a.heldSession()
	if err != nil {
		return err
	}
	return session.SetDPoPNonce(ctx, host, nonce)
}

type proxySource struct {
	a *Authenticator
}

var _ xrpc.DPoPProver = proxySource{}

func (s proxySource) ProveDPoP(ctx context.Context, method, target, accessToken, nonce string) (xrpc.DPoPProof, error) {
	s.a.mu.Lock()
	proxy := s.a.proxy
	s.a.mu.Unlock()
	if proxy == nil || proxy.Prover == nil {
		return xrpc.DPoPProof{}, fmt.Errorf("%w: %w", pdsclient.ErrAuthenticationRequired, ErrNotHeld)
	}
	return proxy.Prover.ProveDPoP(ctx, method, target, accessToken, nonce)
}

type bearerSource struct {
	a *Authenticator
}

var (
	_ pdsclient.TokenSource    = bearerSource{}
	_ pdsclient.TokenRefresher = bearerSource{}
)

func (s bearerSource) AccessToken(ctx context.Context) (string, error) {
	bearer, err := s.a.heldBearer()
	if err != nil {
		return "", err
	}
	return bearer.AccessToken(ctx)
}

func (s bearerSource) RefreshAccessToken(ctx context.Context) (string, error) {
	bearer, err := s.a.heldBearer()
	if err != nil {
		return "", err
	}
	return bearer.RefreshAccessToken(ctx)
}

type Bearer struct {
	store Store

	pdsURL string

	http *http.Client

	log *slog.Logger

	mu sync.Mutex

	access string
}

var (
	_ pdsclient.TokenSource    = (*Bearer)(nil)
	_ pdsclient.TokenRefresher = (*Bearer)(nil)
)

func NewBearerTokens(store Store, pdsURL string, client *http.Client, log *slog.Logger) *Bearer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bearer{store: store, pdsURL: strings.TrimRight(strings.TrimSpace(pdsURL), "/"), http: client, log: log}
}

func (t *Bearer) AccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if pdsclient.TokenLive(t.access) {
		return t.access, nil
	}
	return t.renewLocked(ctx, false)
}

func (t *Bearer) RefreshAccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.renewLocked(ctx, true)
}

func (t *Bearer) renewLocked(ctx context.Context, rejected bool) (string, error) {
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
	if refresh == "" || t.pdsURL == "" {
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
	if err := t.store.Write(ctx, updated); err != nil {
		return "", fmt.Errorf("identity: storing the refreshed session: %w", err)
	}
	t.log.Info("pds_session_refreshed", "did", session.DID.String(), "pds", t.pdsURL, "rejected", rejected)
	return t.access, nil
}

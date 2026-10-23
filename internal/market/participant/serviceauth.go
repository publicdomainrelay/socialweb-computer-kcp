package participant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	ServiceAuthLifetime = 5 * time.Minute

	ServiceAuthRefreshMargin = 30 * time.Second
)

type PDSServiceAuth struct {
	Open func(ctx context.Context) (pds.ServiceAuth, error)

	DID string

	Lifetime time.Duration
}

var _ xrpc.Minter = PDSServiceAuth{}

func (a PDSServiceAuth) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	if a.Open == nil || a.DID == "" {
		return "", errors.New("participant: the PDS service-auth handle names no account")
	}
	minter, err := a.Open(ctx)
	if err != nil {
		return "", err
	}
	lifetime := a.Lifetime
	if lifetime <= 0 {
		lifetime = ServiceAuthLifetime
	}
	return minter.ServiceAuth(ctx, a.DID, audience, lxm, time.Now().Add(lifetime))
}

type SessionServiceAuth struct {
	Endpoint string

	HTTP *http.Client

	Auth xrpc.Auth

	Lifetime time.Duration
}

var _ xrpc.Minter = SessionServiceAuth{}

func (m SessionServiceAuth) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	if strings.TrimSpace(m.Endpoint) == "" {
		return "", errors.New("participant: the session service-auth has no PDS URL")
	}
	lifetime := m.Lifetime
	if lifetime <= 0 {
		lifetime = ServiceAuthLifetime
	}
	client := &xrpc.Client{Host: m.Endpoint, HTTP: m.HTTP, Auth: m.Auth}
	return xrpc.GetServiceAuth(ctx, client, audience, lxm, time.Now().Add(lifetime))
}

type Tokens struct {
	minter xrpc.Minter

	timeout time.Duration

	cache *ttlcache.Cache[string, string]
}

func NewTokens(minter xrpc.Minter, timeout time.Duration) *Tokens {
	return &Tokens{minter: minter, timeout: timeout, cache: ttlcache.New[string, string](ttlcache.Options{})}
}

func (t *Tokens) Token(ctx context.Context, host, lxm string) (string, error) {
	if t == nil || t.minter == nil {
		return "", errors.New("participant: no service-auth minter is configured")
	}
	aud := market.ServiceDIDForHost(host)
	return t.cache.GetOrLoad(ctx, aud+"\x00"+lxm, func(ctx context.Context) (string, time.Duration, error) {
		requested := time.Now().Add(ServiceAuthLifetime)
		callCtx := ctx
		if t.timeout > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, t.timeout)
			defer cancel()
		}
		token, err := t.minter.ServiceAuth(callCtx, aud, lxm)
		if err != nil {
			return "", 0, fmt.Errorf("getServiceAuth %s: %w", aud, err)
		}
		ttl := time.Until(CacheExpiry(token, requested))
		if ttl <= 0 {
			ttl = -1
		}
		return token, ttl, nil
	})
}

func (t *Tokens) Invalidate(host, lxm string) {
	if t == nil {
		return
	}
	t.cache.Delete(market.ServiceDIDForHost(host) + "\x00" + lxm)
}

func (t *Tokens) Source(host, lxm string) xrpc.Source[string] {
	return tokenSource{tokens: t, host: host, lxm: lxm}
}

type tokenSource struct {
	tokens *Tokens

	host string

	lxm string
}

func (s tokenSource) Get(ctx context.Context) (string, error) {
	return s.tokens.Token(ctx, s.host, s.lxm)
}

func (s tokenSource) Invalidate() {
	s.tokens.Invalidate(s.host, s.lxm)
}

func CacheExpiry(token string, requested time.Time) time.Time {
	fallback := requested.Add(-ServiceAuthRefreshMargin)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return fallback
	}
	expires := time.Unix(claims.Exp, 0).Add(-ServiceAuthRefreshMargin)
	if expires.After(requested) {
		return fallback
	}
	return expires
}

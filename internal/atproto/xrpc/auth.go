package xrpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

var ErrNoCredentials = errors.New("xrpc: no credentials are available")

type Source[T any] interface {
	Get(ctx context.Context) (T, error)
	Invalidate()
}

type Auth interface {
	Apply(req *http.Request) error
	Invalidate()
}

type Verdict int

const (
	Done Verdict = iota
	RetryNonce
	RetryAuth
)

type Challenger interface {
	Challenge(resp *http.Response) Verdict
}

func StatusRetry(a Auth) Auth {
	return statusRetry{Auth: a}
}

type statusRetry struct {
	Auth
}

type TokenFunc func(ctx context.Context) (string, error)

func Static(token string) Source[string] {
	return staticSource(token)
}

type staticSource string

func (s staticSource) Get(context.Context) (string, error) { return string(s), nil }

func (staticSource) Invalidate() {}

func Refreshing(get, refresh TokenFunc) Source[string] {
	return &refreshingSource{get: get, refresh: refresh}
}

type refreshingSource struct {
	get TokenFunc

	refresh TokenFunc

	stale atomic.Bool
}

func (s *refreshingSource) Get(ctx context.Context) (string, error) {
	if s.stale.Swap(false) {
		if s.refresh == nil {
			return "", ErrNoCredentials
		}
		if _, err := s.refresh(ctx); err != nil {
			return "", err
		}
	}
	if s.get == nil {
		return "", ErrNoCredentials
	}
	return s.get(ctx)
}

func (s *refreshingSource) Invalidate() { s.stale.Store(true) }

type Bearer struct {
	Tokens Source[string]
}

func (b *Bearer) Apply(req *http.Request) error {
	return applyBearer(req, b.Tokens)
}

func (b *Bearer) Invalidate() { b.Tokens.Invalidate() }

type ServiceAuth struct {
	Tokens Source[string]
}

func NewServiceAuth(minter Minter, aud, lxm string) *ServiceAuth {
	return &ServiceAuth{Tokens: &mintedSource{minter: minter, aud: aud, lxm: lxm}}
}

func (s *ServiceAuth) Apply(req *http.Request) error {
	return applyBearer(req, s.Tokens)
}

func (s *ServiceAuth) Invalidate() { s.Tokens.Invalidate() }

type mintedSource struct {
	minter Minter

	aud string

	lxm string
}

func (m *mintedSource) Get(ctx context.Context) (string, error) {
	if m.minter == nil {
		return "", ErrNoCredentials
	}
	return m.minter.ServiceAuth(ctx, m.aud, m.lxm)
}

func (*mintedSource) Invalidate() {}

func applyBearer(req *http.Request, tokens Source[string]) error {
	if tokens == nil {
		return ErrNoCredentials
	}
	token, err := tokens.Get(req.Context())
	if err != nil {
		return err
	}
	if token == "" {
		return ErrNoCredentials
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

type DPoPProof struct {
	AccessToken string

	Proof string
}

type DPoPProver interface {
	ProveDPoP(ctx context.Context, method, target, accessToken, nonce string) (DPoPProof, error)
}

type DPoPProverFunc func(ctx context.Context, method, target, accessToken, nonce string) (DPoPProof, error)

func (f DPoPProverFunc) ProveDPoP(ctx context.Context, method, target, accessToken, nonce string) (DPoPProof, error) {
	return f(ctx, method, target, accessToken, nonce)
}

type DPoP struct {
	Tokens Source[string]

	Prover DPoPProver

	mu sync.Mutex

	nonces map[string]string
}

func (d *DPoP) Apply(req *http.Request) error {
	if d.Prover == nil {
		return ErrNoCredentials
	}
	ctx := req.Context()
	token := ""
	if d.Tokens != nil {
		var err error
		token, err = d.Tokens.Get(ctx)
		if err != nil {
			return err
		}
	}
	proof, err := d.Prover.ProveDPoP(ctx, req.Method, req.URL.String(), token, d.nonce(origin(req)))
	if err != nil {
		return err
	}
	if proof.AccessToken != "" {
		token = proof.AccessToken
	}
	if token == "" {
		return ErrNoCredentials
	}
	req.Header.Set("Authorization", "DPoP "+token)
	if proof.Proof != "" {
		req.Header.Set("DPoP", proof.Proof)
	}
	return nil
}

func (d *DPoP) Invalidate() {
	if d.Tokens != nil {
		d.Tokens.Invalidate()
	}
}

func (d *DPoP) Challenge(resp *http.Response) Verdict {
	nonce := resp.Header.Get("DPoP-Nonce")
	if nonce != "" && resp.Request != nil {
		d.mu.Lock()
		if d.nonces == nil {
			d.nonces = map[string]string{}
		}
		d.nonces[origin(resp.Request)] = nonce
		d.mu.Unlock()
	}
	invalidToken := strings.Contains(resp.Header.Get("WWW-Authenticate"), "invalid_token")
	if nonce != "" && !invalidToken && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) {
		return RetryNonce
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return RetryAuth
	}
	return Done
}

func (d *DPoP) nonce(origin string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nonces[origin]
}

func origin(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Scheme + "://" + req.URL.Host
}

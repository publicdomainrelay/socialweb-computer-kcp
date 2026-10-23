package pdsclient

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	defaultCallTimeout   = 30 * time.Second
	maxJSONResponseBytes = 64 << 20
	maxRepoBytes         = 256 << 20
	maxListRecordsLimit  = 100
)

type authStore struct {
	mu      sync.RWMutex
	session Session
	present bool
}

func (a *authStore) set(session Session) {
	a.mu.Lock()
	a.session = session
	a.present = true
	a.mu.Unlock()
}

func (a *authStore) snapshot() Session {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.present {
		return Session{}
	}
	return a.session
}

func (a *authStore) clear() {
	a.mu.Lock()
	a.session = Session{}
	a.present = false
	a.mu.Unlock()
}

type xrpcClient struct {
	base        *url.URL
	httpClient  *http.Client
	timeout     time.Duration
	auth        xrpc.Auth
	session     *authStore
	expectedDID syntax.DID
}

func newXRPCClient(base *url.URL, httpClient *http.Client, tokens TokenSource, dpop DPoPSource, auth xrpc.Auth, timeout time.Duration) *xrpcClient {
	if auth == nil && tokens != nil {
		auth = TokenAuth(tokens, dpop)
	}
	return &xrpcClient{base: base, httpClient: httpClient, timeout: opt.Positive(timeout, defaultCallTimeout), auth: auth, session: &authStore{}}
}

func TokenAuth(tokens TokenSource, dpop DPoPSource) xrpc.Auth {
	if dpop == nil {
		dpop, _ = tokens.(DPoPSource)
	}
	var refresh xrpc.TokenFunc
	if refresher, ok := tokens.(TokenRefresher); ok {
		refresh = refresher.RefreshAccessToken
	}
	source := xrpc.Refreshing(tokens.AccessToken, refresh)
	if dpop == nil {
		return &xrpc.Bearer{Tokens: source}
	}
	return &xrpc.DPoP{Tokens: source, Prover: dpopProver{source: dpop}}
}

type dpopProver struct {
	source DPoPSource
}

func (p dpopProver) ProveDPoP(ctx context.Context, method, target, accessToken, nonce string) (xrpc.DPoPProof, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return xrpc.DPoPProof{}, fmt.Errorf("%w: the dpop target %q: %v", ErrInvalidRequest, target, err)
	}
	if setter, ok := p.source.(DPoPNonceSetter); ok && nonce != "" {
		if err := setter.SetDPoPNonce(ctx, parsed.Host, nonce); err != nil {
			return xrpc.DPoPProof{}, err
		}
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	proof, err := p.source.DPoPProof(ctx, method, parsed.String(), accessToken)
	if err != nil {
		return xrpc.DPoPProof{}, fmt.Errorf("pdsclient: dpop proof: %w", err)
	}
	return xrpc.DPoPProof{Proof: proof}, nil
}

func bearer(token string) xrpc.Auth {
	return &xrpc.Bearer{Tokens: xrpc.Static(token)}
}

func (c *xrpcClient) credential() (xrpc.Auth, error) {
	if c.auth != nil {
		return c.auth, nil
	}
	if token := c.session.snapshot().AccessJWT; token != "" {
		return bearer(token), nil
	}
	return nil, fmt.Errorf("%w: no access token is available", ErrAuthenticationRequired)
}

func (c *xrpcClient) client(auth xrpc.Auth, limit int64) (*xrpc.Client, error) {
	if c == nil || c.base == nil {
		return nil, fmt.Errorf("%w: client has no base URL", ErrInvalidConfig)
	}
	if c.httpClient == nil {
		return nil, fmt.Errorf("%w: client has no HTTP client", ErrInvalidConfig)
	}
	return &xrpc.Client{Host: strings.TrimRight(c.base.String(), "/"), HTTP: c.httpClient, Auth: auth, Limit: limit}, nil
}

func (c *xrpcClient) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

func (c *xrpcClient) get(ctx context.Context, nsid string, query url.Values, auth xrpc.Auth, limit int64) ([]byte, error) {
	client, err := c.client(auth, limit)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	reply, err := xrpc.Query[xrpc.Reply](callCtx, client, nsid, query)
	if err != nil {
		return nil, convertError(err)
	}
	return reply.Body, nil
}

func (c *xrpcClient) getJSON(ctx context.Context, nsid string, query url.Values, auth xrpc.Auth, out any) error {
	body, err := c.get(ctx, nsid, query, auth, maxJSONResponseBytes)
	if err != nil {
		return err
	}
	return decodeJSONBody(body, out)
}

func (c *xrpcClient) postJSON(ctx context.Context, nsid string, in any, auth xrpc.Auth, out any) error {
	client, err := c.client(auth, maxJSONResponseBytes)
	if err != nil {
		return err
	}
	if in == nil {
		in = map[string]any{}
	}
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	reply, err := xrpc.Procedure[any, xrpc.Reply](callCtx, client, nsid, in)
	if err != nil {
		return convertError(err)
	}
	if out == nil {
		return nil
	}
	return decodeJSONBody(reply.Body, out)
}

func decodeJSONBody(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: decoding response: %v", ErrUnexpectedResponse, err)
	}
	return nil
}

func convertError(err error) error {
	if failure, ok := errors.AsType[*xrpc.Error](err); ok {
		return &XRPCError{StatusCode: failure.Status, Code: cmp.Or(failure.Name, "HTTPError"),
			Message: cmp.Or(failure.Message, http.StatusText(failure.Status))}
	}
	if errors.Is(err, xrpc.ErrNoCredentials) {
		return fmt.Errorf("%w: %v", ErrAuthenticationRequired, err)
	}
	return err
}

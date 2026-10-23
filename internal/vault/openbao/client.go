package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	api "github.com/openbao/openbao/api/v2"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

var (
	ErrNotFound = errors.New("openbao: not configured (HTTP 404)")

	ErrUnauthorized = errors.New("openbao: the token is not accepted (HTTP 401)")

	ErrForbidden = errors.New("openbao: denied by policy (HTTP 403)")

	ErrTransport = errors.New("openbao: transport failure")

	ErrNoCA = errors.New("openbao: the supplied CA carries no CERTIFICATE PEM block")
)

const (
	IdleConnTimeout = 90 * time.Second

	maxBody = 32 << 20
)

type ResponseError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("openbao: %s /v1/%s -> HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (e *ResponseError) HTTPStatus() int { return e.Status }

func (e *ResponseError) Is(target error) bool {
	switch e.Status {
	case http.StatusNotFound:
		return target == ErrNotFound
	case http.StatusUnauthorized:
		return target == ErrUnauthorized
	case http.StatusForbidden:
		return target == ErrForbidden
	default:
		return false
	}
}

func IsAuthError(err error) bool {
	var re *ResponseError
	return errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Status == http.StatusForbidden)
}

type tokenSource interface {
	Get(ctx context.Context) (string, error)
	Invalidate()
}

type staticToken string

func (t staticToken) Get(context.Context) (string, error) { return string(t), nil }

func (staticToken) Invalidate() {}

type Client struct {
	addr string

	http *http.Client

	tls *tls.Config

	timeout time.Duration

	api *api.Client

	token tokenSource

	renews bool

	err error
}

type Option func(*Client)

func CertPoolFromPEM(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, ErrNoCA
	}
	return pool, nil
}

// ponytail: one trust root per client, no system pool; a caller that needs the public roots too can fold them in before WithCACertPool.
func WithCACertPool(pool *x509.CertPool) Option {
	return func(c *Client) {
		c.tlsConfig().RootCAs = pool
	}
}

func WithClientCertificate(cert tls.Certificate) Option {
	return func(c *Client) {
		c.tlsConfig().Certificates = []tls.Certificate{cert}
	}
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		c.timeout = timeout
	}
}

func (c *Client) tlsConfig() *tls.Config {
	if c.tls == nil {
		c.tls = &tls.Config{MinVersion: tls.VersionTLS12}
		c.http = &http.Client{Transport: &http.Transport{TLSClientConfig: c.tls, IdleConnTimeout: IdleConnTimeout}}
	}
	return c.tls
}

func New(addr, token string, opts ...Option) *Client {
	c := &Client{addr: strings.TrimRight(strings.TrimSpace(addr), "/"), http: http.DefaultClient, token: staticToken(token)}
	for _, o := range opts {
		o(c)
	}
	c.api, c.err = newAPI(c.addr, c.http, c.timeout)
	return c
}

func newAPI(addr string, hc *http.Client, timeout time.Duration) (*api.Client, error) {
	client, err := api.NewClient(&api.Config{Address: addr, HttpClient: hc, MaxRetries: 0, Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("%w: the client for %s: %v", ErrTransport, addr, err)
	}
	client.ClearToken()
	client.ClearNamespace()
	return client, nil
}

func (c *Client) with(token tokenSource, renews bool) *Client {
	return &Client{addr: c.addr, http: c.http, tls: c.tls, timeout: c.timeout, api: c.api, err: c.err, token: token, renews: renews}
}

func (c *Client) anonymous() *Client {
	return c.with(staticToken(""), false)
}

func (c *Client) WithToken(token string) *Client {
	return c.with(staticToken(token), false)
}

func (c *Client) Address() string { return c.addr }

func leaseUntil(lease time.Duration) time.Time {
	if lease <= 0 {
		return time.Time{}
	}
	return time.Now().Add(lease)
}

func (c *Client) leased(ctx context.Context, login func(context.Context) (loginAuth, error)) (*Client, error) {
	src := lease.New(func(ctx context.Context) (string, time.Time, error) {
		auth, err := login(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		return auth.token, leaseUntil(auth.lease), nil
	})
	if _, err := src.Get(ctx); err != nil {
		return nil, err
	}
	return c.with(src, true), nil
}

func (c *Client) LoginCert(ctx context.Context, mount, role string) (*Client, error) {
	path := "auth/" + strings.Trim(mount, "/") + "/login"
	anonymous := c.anonymous()
	return c.leased(ctx, func(ctx context.Context) (loginAuth, error) {
		return anonymous.login(ctx, path, map[string]any{"name": role})
	})
}

func (c *Client) LoginAppRole(ctx context.Context, mount, roleID, secretID string) (*Client, error) {
	if mount = strings.Trim(mount, "/"); mount == "" {
		mount = "approle"
	}
	path := "auth/" + mount + "/login"
	anonymous := c.anonymous()
	return c.leased(ctx, func(ctx context.Context) (loginAuth, error) {
		return anonymous.login(ctx, path, map[string]any{"role_id": roleID, "secret_id": secretID})
	})
}

func (c *Client) LoginJWT(ctx context.Context, mount, role, jwt string) (*Client, error) {
	auth, err := c.anonymous().login(ctx, "auth/"+strings.Trim(mount, "/")+"/login", map[string]any{"role": role, "jwt": jwt})
	if err != nil {
		return nil, err
	}
	return c.WithToken(auth.token), nil
}

type loginAuth struct {
	token string

	accessor string

	lease time.Duration

	renewable bool
}

func (c *Client) login(ctx context.Context, path string, body map[string]any) (loginAuth, error) {
	status, raw, err := c.send(ctx, http.MethodPost, path, body, "")
	if err != nil {
		return loginAuth{}, err
	}
	if status < 200 || status > 299 {
		return loginAuth{}, &ResponseError{Method: http.MethodPost, Path: path, Status: status, Body: strings.TrimSpace(string(raw))}
	}
	return decodeAuth(path, raw, true)
}

func decodeAuth(path string, raw []byte, needToken bool) (loginAuth, error) {
	var payload struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			Accessor      string `json:"accessor"`
			LeaseDuration int64  `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return loginAuth{}, fmt.Errorf("openbao: %s returned a body that is not JSON: %v", path, err)
	}
	if needToken && payload.Auth.ClientToken == "" {
		return loginAuth{}, fmt.Errorf("openbao: %s answered without a client token", path)
	}
	return loginAuth{token: payload.Auth.ClientToken, accessor: payload.Auth.Accessor,
		lease: time.Duration(payload.Auth.LeaseDuration) * time.Second, renewable: payload.Auth.Renewable}, nil
}

func (c *Client) RevokeSelf(ctx context.Context) error {
	_, err := c.topLevel(ctx, http.MethodPost, "auth/token/revoke-self", nil)
	c.token.Invalidate()
	return err
}

func (c *Client) do(ctx context.Context, method, path string, in any) (json.RawMessage, error) {
	status, body, err := c.request(ctx, method, path, in)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, &ResponseError{Method: method, Path: path, Status: status, Body: strings.TrimSpace(string(body))}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("openbao: %s /v1/%s returned a body that is not JSON: %v (%s)", method, path, err, strings.TrimSpace(string(body)))
	}
	return envelope.Data, nil
}

func (c *Client) topLevel(ctx context.Context, method, path string, in any) (json.RawMessage, error) {
	status, body, err := c.request(ctx, method, path, in)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, &ResponseError{Method: method, Path: path, Status: status, Body: strings.TrimSpace(string(body))}
	}
	return json.RawMessage(bytes.TrimSpace(body)), nil
}

func (c *Client) request(ctx context.Context, method, path string, in any) (int, []byte, error) {
	token, err := c.token.Get(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: the token for %s /v1/%s: %v", ErrTransport, method, path, err)
	}
	status, body, err := c.send(ctx, method, path, in, token)
	if err != nil || !c.renews || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return status, body, err
	}
	c.token.Invalidate()
	fresh, ferr := c.token.Get(ctx)
	if ferr != nil || fresh == token {
		return status, body, nil
	}
	return c.send(ctx, method, path, in, fresh)
}

func (c *Client) send(ctx context.Context, method, path string, in any, token string) (int, []byte, error) {
	if c.err != nil {
		return 0, nil, c.err
	}
	clean, query, _ := strings.Cut(strings.TrimPrefix(path, "/"), "?")
	r := c.api.NewRequest(method, "/v1/"+clean)
	r.ClientToken = token
	r.WrapTTL = ""
	if query != "" {
		values, err := url.ParseQuery(query)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: the query of %s /v1/%s: %v", ErrTransport, method, path, err)
		}
		r.Params = values
	}
	if in != nil {
		if err := r.SetJSONBody(in); err != nil {
			return 0, nil, fmt.Errorf("%w: encode %s /v1/%s: %v", ErrTransport, method, path, err)
		}
	}
	resp, err := c.api.RawRequestWithContext(ctx, r)
	if resp == nil || resp.Response == nil {
		if err == nil {
			err = errors.New("no response")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, err
		}
		return 0, nil, fmt.Errorf("%w: %s /v1/%s: %v", ErrTransport, method, path, err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if rerr != nil {
		return 0, nil, fmt.Errorf("%w: reading %s /v1/%s: %v", ErrTransport, method, path, rerr)
	}
	return resp.StatusCode, body, nil
}

func (c *Client) Raw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	return c.request(ctx, method, path, body)
}

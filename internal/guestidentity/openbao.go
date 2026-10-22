package guestidentity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	DefaultTimeout = 10 * time.Second

	ownTokenRenewBefore = time.Minute

	maximumReplyBody = 1 << 20
)

type CertLogin struct {
	Mount string

	Role string
}

type Endpoint struct {
	Address string

	CAPEM []byte

	ClientCert tls.Certificate

	CertLogin *CertLogin
}

type Options struct {
	Mount string

	Endpoint func(context.Context) (Endpoint, error)

	Timeout time.Duration
}

type Reply struct {
	Status int

	Body json.RawMessage
}

func (r Reply) OK() bool { return r.Status >= 200 && r.Status < 300 }

type Client struct {
	mount    string
	endpoint func(context.Context) (Endpoint, error)
	timeout  time.Duration

	mu      sync.Mutex
	client  *http.Client
	clientK string
	own     string
	ownKey  string
	renewAt time.Time
}

func EndpointFromServer(st v1alpha1.OpenBaoServerStatus, client string) (Endpoint, error) {
	refs, err := openbaoctl.ClientRefs(st, client)
	if err != nil {
		return Endpoint{}, err
	}
	if st.Address == "" || st.Refs == nil {
		return Endpoint{}, fmt.Errorf("guestidentity: the OpenBaoServer reports no address or certificate authority")
	}
	ca, err := os.ReadFile(openbaoctl.FileRef(st.Refs.CA))
	if err != nil {
		return Endpoint{}, fmt.Errorf("guestidentity: reading the OpenBao server CA: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(openbaoctl.FileRef(refs.ClientCert), openbaoctl.FileRef(refs.ClientKey))
	if err != nil {
		return Endpoint{}, fmt.Errorf("guestidentity: loading the OpenBao client certificate: %w", err)
	}
	return Endpoint{
		Address:    st.Address,
		CAPEM:      ca,
		ClientCert: cert,
		CertLogin:  &CertLogin{Mount: refs.CertAuthMount, Role: refs.CertAuthRole},
	}, nil
}

func NewClient(opts Options) (*Client, error) {
	if !ValidMount(opts.Mount) {
		return nil, fmt.Errorf("guestidentity: mount %q is not an auth mount name", opts.Mount)
	}
	if opts.Endpoint == nil {
		return nil, errors.New("guestidentity: no OpenBao endpoint resolver")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{mount: opts.Mount, endpoint: opts.Endpoint, timeout: timeout}, nil
}

func (c *Client) Mount() string { return c.mount }

func (c *Client) Step(ctx context.Context, step LoginStep, body map[string]string) (Reply, error) {
	fields, ok := loginFields[step]
	if !ok {
		return Reply{}, fmt.Errorf("guestidentity: %q is not a guest login step", step)
	}
	if len(body) != len(fields) {
		return Reply{}, fmt.Errorf("guestidentity: the %s step takes exactly %s", step, strings.Join(fields, ", "))
	}
	for _, name := range fields {
		if body[name] == "" || len(body[name]) > maximumLoginField {
			return Reply{}, fmt.Errorf("guestidentity: %s must be a string of 1 to %d bytes", name, maximumLoginField)
		}
	}
	if !ValidComputeVM(body["role"]) {
		return Reply{}, fmt.Errorf("guestidentity: role %q is not a ComputeVM name", body["role"])
	}
	return c.send(ctx, http.MethodPost, "auth/"+c.mount+"/"+string(step), "", body)
}

func (c *Client) Token(ctx context.Context, role, vaultToken string) (Reply, error) {
	if !ValidRole(role) {
		return Reply{}, fmt.Errorf("guestidentity: role %q is not a guest role name", role)
	}
	if vaultToken == "" || len(vaultToken) > 512 {
		return Reply{}, errors.New("guestidentity: the guest vault token is empty or too long")
	}
	return c.send(ctx, http.MethodGet, "identity/oidc/token/"+role, vaultToken, nil)
}

func (c *Client) Discovery(ctx context.Context) (Reply, error) {
	return c.send(ctx, http.MethodGet, "identity/oidc/.well-known/openid-configuration", "", nil)
}

func (c *Client) Keys(ctx context.Context) (Reply, error) {
	return c.send(ctx, http.MethodGet, "identity/oidc/.well-known/keys", "", nil)
}

func (c *Client) BoundHostKey(ctx context.Context, computevm string) (string, error) {
	if !ValidComputeVM(computevm) {
		return "", fmt.Errorf("guestidentity: %q is not a ComputeVM name", computevm)
	}
	reply, err := c.authed(ctx, http.MethodGet, "auth/"+c.mount+"/role/"+computevm, nil)
	if err != nil {
		return "", err
	}
	if reply.Status != http.StatusOK {
		return "", nil
	}
	var envelope struct {
		Data struct {
			Bound   bool   `json:"bound"`
			HostKey string `json:"host_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reply.Body, &envelope); err != nil {
		return "", fmt.Errorf("guestidentity: reading the bound host key of %s: %w", computevm, err)
	}
	if !envelope.Data.Bound || envelope.Data.HostKey == "" {
		return "", nil
	}
	return envelope.Data.HostKey, nil
}

func (c *Client) ConfigureIssuer(ctx context.Context, ingressURL string) error {
	issuer, err := cleanIngressURL(ingressURL)
	if err != nil {
		return err
	}
	held, err := c.authed(ctx, http.MethodGet, "identity/oidc/config", nil)
	if err != nil {
		return err
	}
	if held.Status == http.StatusOK {
		var envelope struct {
			Data struct {
				Issuer string `json:"issuer"`
			} `json:"data"`
		}
		if json.Unmarshal(held.Body, &envelope) == nil && envelope.Data.Issuer == issuer {
			return nil
		}
	}
	reply, err := c.authed(ctx, http.MethodPut, "identity/oidc/config", map[string]string{"issuer": issuer})
	if err != nil {
		return err
	}
	if !reply.OK() {
		return fmt.Errorf("guestidentity: setting the OIDC issuer returned HTTP %d: %s", reply.Status, bodyPreview(reply.Body))
	}
	return nil
}

func (c *Client) authed(ctx context.Context, method, path string, body any) (Reply, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.ownToken(ctx)
		if err != nil {
			return Reply{}, err
		}
		reply, err := c.send(ctx, method, path, token, body)
		if err != nil {
			return Reply{}, err
		}
		if (reply.Status == http.StatusForbidden || reply.Status == http.StatusUnauthorized) && attempt == 0 {
			c.forgetOwn()
			continue
		}
		return reply, nil
	}
}

func (c *Client) ownToken(ctx context.Context) (string, error) {
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return "", err
	}
	if endpoint.CertLogin == nil {
		return "", errors.New("guestidentity: the OpenBao endpoint names no certificate login for this process")
	}
	if !ValidMount(endpoint.CertLogin.Mount) || endpoint.CertLogin.Role == "" {
		return "", errors.New("guestidentity: the OpenBao certificate login mount or role is not a name")
	}
	key, err := endpointKey(endpoint)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.own != "" && c.ownKey == key && time.Now().Before(c.renewAt) {
		token := c.own
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	reply, err := c.send(ctx, http.MethodPost, "auth/"+endpoint.CertLogin.Mount+"/login", "", map[string]string{"name": endpoint.CertLogin.Role})
	if err != nil {
		return "", err
	}
	if !reply.OK() {
		return "", fmt.Errorf("guestidentity: certificate login as %s returned HTTP %d: %s", endpoint.CertLogin.Role, reply.Status, bodyPreview(reply.Body))
	}
	var payload struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(reply.Body, &payload); err != nil || payload.Auth.ClientToken == "" {
		return "", fmt.Errorf("guestidentity: certificate login as %s answered without a client token", endpoint.CertLogin.Role)
	}
	lease := time.Duration(payload.Auth.LeaseDuration) * time.Second
	renew := lease - ownTokenRenewBefore
	if renew < lease/2 {
		renew = lease / 2
	}
	if renew < 0 {
		renew = 0
	}
	c.mu.Lock()
	c.own, c.ownKey, c.renewAt = payload.Auth.ClientToken, key, time.Now().Add(renew)
	c.mu.Unlock()
	return payload.Auth.ClientToken, nil
}

func (c *Client) forgetOwn() {
	c.mu.Lock()
	c.own, c.ownKey, c.renewAt = "", "", time.Time{}
	c.mu.Unlock()
}

func (c *Client) send(ctx context.Context, method, path, vaultToken string, body any) (Reply, error) {
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return Reply{}, err
	}
	client, err := c.httpClient(endpoint)
	if err != nil {
		return Reply{}, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return Reply{}, fmt.Errorf("guestidentity: encoding the %s request: %w", path, err)
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint.Address, "/")+"/v1/"+path, reader)
	if err != nil {
		return Reply{}, fmt.Errorf("guestidentity: building the %s request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if vaultToken != "" {
		req.Header.Set(VaultTokenHeader, vaultToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("guestidentity: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maximumReplyBody))
	if err != nil {
		return Reply{}, fmt.Errorf("guestidentity: reading the %s reply: %w", path, err)
	}
	reply := Reply{Status: resp.StatusCode}
	if len(bytes.TrimSpace(raw)) > 0 {
		reply.Body = json.RawMessage(append([]byte(nil), raw...))
	}
	return reply, nil
}

func (c *Client) httpClient(e Endpoint) (*http.Client, error) {
	address, err := parseAddress(e.Address)
	if err != nil {
		return nil, err
	}
	key, err := endpointKey(e)
	if err != nil {
		return nil, err
	}
	key += "\x00" + address
	c.mu.Lock()
	if c.client != nil && c.clientK == key {
		client := c.client
		c.mu.Unlock()
		return client, nil
	}
	c.mu.Unlock()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(e.CAPEM) > 0 {
		pool, err := openbao.CertPoolFromPEM(e.CAPEM)
		if err != nil {
			return nil, fmt.Errorf("guestidentity: %w", err)
		}
		tlsConfig.RootCAs = pool
	}
	if len(e.ClientCert.Certificate) > 0 {
		tlsConfig.Certificates = []tls.Certificate{e.ClientCert}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 4}}
	c.mu.Lock()
	c.client, c.clientK = client, key
	c.mu.Unlock()
	return client, nil
}

func endpointKey(e Endpoint) (string, error) {
	sum := sha256.New()
	sum.Write(e.CAPEM)
	for _, cert := range e.ClientCert.Certificate {
		sum.Write(cert)
	}
	login := ""
	if e.CertLogin != nil {
		login = e.CertLogin.Mount + "\x00" + e.CertLogin.Role
	}
	return e.Address + "\x00" + hex.EncodeToString(sum.Sum(nil)) + "\x00" + login, nil
}

func parseAddress(address string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("guestidentity: OpenBao address %q is not a URL", address)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("guestidentity: OpenBao address %q is not http or https", address)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("guestidentity: OpenBao address %q carries a user, path, query or fragment", address)
	}
	return u.Scheme + "://" + u.Host, nil
}

func cleanIngressURL(ingressURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(ingressURL), "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("guestidentity: ingress URL %q is not an absolute http or https URL", ingressURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("guestidentity: ingress URL %q carries a user, query or fragment", ingressURL)
	}
	return trimmed, nil
}

func bodyPreview(body json.RawMessage) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		return text[:200]
	}
	return text
}

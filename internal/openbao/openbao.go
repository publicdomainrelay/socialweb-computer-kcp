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
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("openbao: not configured (HTTP 404)")

	ErrForbidden = errors.New("openbao: denied by policy (HTTP 403)")

	ErrTransport = errors.New("openbao: transport failure")
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

func (e *ResponseError) Is(target error) bool {
	switch e.Status {
	case http.StatusNotFound:
		return target == ErrNotFound
	case http.StatusForbidden:
		return target == ErrForbidden
	default:
		return false
	}
}

type Client struct {
	addr  string
	token string
	http  *http.Client
	tls   *tls.Config
}

type Option func(*Client)

var ErrNoCA = errors.New("openbao: the supplied CA carries no CERTIFICATE PEM block")

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

// ponytail: one client identity per vault, shared by every consumer; a per-consumer certificate with its own policy is the upgrade if the root token stops being the only authorisation.
func WithClientCertificate(cert tls.Certificate) Option {
	return func(c *Client) {
		c.tlsConfig().Certificates = []tls.Certificate{cert}
	}
}

func (c *Client) tlsConfig() *tls.Config {
	if c.tls == nil {
		c.tls = &tls.Config{MinVersion: tls.VersionTLS12}
		c.http = &http.Client{Transport: &http.Transport{TLSClientConfig: c.tls, IdleConnTimeout: IdleConnTimeout}}
	}
	return c.tls
}

const IdleConnTimeout = 90 * time.Second

func New(addr, token string, opts ...Option) *Client {
	c := &Client{
		addr:  strings.TrimRight(addr, "/"),
		token: token,
		http:  http.DefaultClient,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type Health struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	Standby     bool   `json:"standby"`
	Version     string `json:"version"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	status, body, err := c.request(ctx, http.MethodGet, "sys/health", nil)
	if err != nil {
		return Health{}, err
	}
	switch status {
	case 200, 429, 472, 473, 501, 503:
	default:
		return Health{}, &ResponseError{Method: http.MethodGet, Path: "sys/health", Status: status, Body: strings.TrimSpace(string(body))}
	}
	var h Health
	if err := json.Unmarshal(body, &h); err != nil {
		return Health{}, fmt.Errorf("openbao: sys/health returned a body that is not JSON: %v (%s)", err, strings.TrimSpace(string(body)))
	}
	return h, nil
}

func (c *Client) Initialized(ctx context.Context) (bool, error) {
	h, err := c.Health(ctx)
	if err != nil {
		return false, err
	}
	return h.Initialized, nil
}

func (c *Client) Sealed(ctx context.Context) (bool, error) {
	h, err := c.Health(ctx)
	if err != nil {
		return false, err
	}
	return h.Sealed, nil
}

type InitResult struct {
	Keys       []string
	KeysBase64 []string
	RootToken  string
}

func (c *Client) Init(ctx context.Context, shares, threshold int) (InitResult, error) {
	raw, err := c.topLevel(ctx, http.MethodPut, "sys/init", map[string]any{
		"secret_shares":    shares,
		"secret_threshold": threshold,
	})
	if err != nil {
		return InitResult{}, err
	}
	var payload struct {
		Keys       []string `json:"keys"`
		KeysBase64 []string `json:"keys_base64"`
		RootToken  string   `json:"root_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return InitResult{}, fmt.Errorf("openbao: sys/init returned a body that is not JSON: %v (%s)", err, strings.TrimSpace(string(raw)))
	}
	return InitResult{Keys: payload.Keys, KeysBase64: payload.KeysBase64, RootToken: payload.RootToken}, nil
}

func (c *Client) Unseal(ctx context.Context, key string) (bool, error) {
	raw, err := c.topLevel(ctx, http.MethodPut, "sys/unseal", map[string]any{"key": key})
	if err != nil {
		return true, err
	}
	var payload struct {
		Sealed bool `json:"sealed"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return true, fmt.Errorf("openbao: sys/unseal returned a body that is not JSON: %v (%s)", err, strings.TrimSpace(string(raw)))
	}
	return payload.Sealed, nil
}

func (c *Client) EnableKV(ctx context.Context, path string, version int) error {
	body := map[string]any{"type": "kv"}
	if version == 2 {
		body["options"] = map[string]any{"version": strconv.Itoa(version)}
	}
	if err := c.enableMount(ctx, path, body); err != nil {
		return err
	}
	if version != 2 {
		return nil
	}
	return c.waitKVUpgraded(ctx, path)
}

const kvUpgradePoll = 50 * time.Millisecond

const kvUpgradeLimit = 30 * time.Second

func (c *Client) waitKVUpgraded(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, kvUpgradeLimit)
	defer cancel()
	for {
		_, err := c.do(ctx, http.MethodGet, strings.Trim(path, "/")+"/config", nil)
		var re *ResponseError
		if err == nil || !errors.As(err, &re) || !strings.Contains(re.Body, "Upgrading from non-versioned to versioned data") {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("openbao: kv mount %q still upgrading to version 2: %w", path, ctx.Err())
		case <-time.After(kvUpgradePoll):
		}
	}
}

func (c *Client) EnableTransit(ctx context.Context, path string) error {
	return c.enableMount(ctx, path, map[string]any{"type": "transit"})
}

func (c *Client) MountPKI(ctx context.Context, path string) error {
	return c.enableMount(ctx, path, map[string]any{"type": "pki"})
}

func (c *Client) enableMount(ctx context.Context, path string, body map[string]any) error {
	_, err := c.do(ctx, http.MethodPut, "sys/mounts/"+strings.Trim(path, "/"), body)
	return err
}

func (c *Client) PutPolicy(ctx context.Context, name, hcl string) error {
	_, err := c.do(ctx, http.MethodPut, "sys/policies/acl/"+strings.Trim(name, "/"), map[string]any{"policy": hcl})
	return err
}

func (c *Client) GetPolicy(ctx context.Context, name string) (string, error) {
	raw, err := c.do(ctx, http.MethodGet, "sys/policies/acl/"+strings.Trim(name, "/"), nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		Policy string `json:"policy"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("openbao: reading policy %q returned a body that is not JSON: %v (%s)", name, err, strings.TrimSpace(string(raw)))
	}
	return payload.Policy, nil
}

func (c *Client) PutKV(ctx context.Context, path string, data map[string]any) error {
	_, err := c.do(ctx, http.MethodPut, strings.Trim(path, "/"), map[string]any{"data": data})
	return err
}

func (c *Client) GetKV(ctx context.Context, path string) (map[string]any, error) {
	raw, err := c.do(ctx, http.MethodGet, strings.Trim(path, "/"), nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data     map[string]any `json:"data"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("openbao: reading %q returned a body that is not JSON: %v (%s)", path, err, strings.TrimSpace(string(raw)))
	}
	return payload.Data, nil
}

func (c *Client) EnableAppRole(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPut, "sys/auth/approle", map[string]any{"type": "approle"})
	return err
}

func (c *Client) WriteAppRole(ctx context.Context, name string, policies []string, ttl string) error {
	body := map[string]any{"token_policies": policies}
	if ttl != "" {
		body["token_ttl"] = ttl
	}
	_, err := c.do(ctx, http.MethodPut, "auth/approle/role/"+strings.Trim(name, "/"), body)
	return err
}

func (c *Client) AppRoleRoleID(ctx context.Context, name string) (string, error) {
	raw, err := c.do(ctx, http.MethodGet, "auth/approle/role/"+strings.Trim(name, "/")+"/role-id", nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		RoleID string `json:"role_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("openbao: reading the role id for %q returned a body that is not JSON: %v (%s)", name, err, strings.TrimSpace(string(raw)))
	}
	return payload.RoleID, nil
}

func (c *Client) AppRoleSecretID(ctx context.Context, name string) (string, error) {
	raw, err := c.do(ctx, http.MethodPut, "auth/approle/role/"+strings.Trim(name, "/")+"/secret-id", map[string]any{})
	if err != nil {
		return "", err
	}
	var payload struct {
		SecretID string `json:"secret_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("openbao: reading the secret id for %q returned a body that is not JSON: %v (%s)", name, err, strings.TrimSpace(string(raw)))
	}
	return payload.SecretID, nil
}

func (c *Client) LoginAppRole(ctx context.Context, roleID, secretID string) (string, error) {
	status, body, err := c.request(ctx, http.MethodPost, "auth/approle/login", map[string]any{
		"role_id":   roleID,
		"secret_id": secretID,
	})
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", &ResponseError{Method: http.MethodPost, Path: "auth/approle/login", Status: status, Body: strings.TrimSpace(string(body))}
	}
	var payload struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("openbao: auth/approle/login returned a body that is not JSON: %v (%s)", err, strings.TrimSpace(string(body)))
	}
	if payload.Auth.ClientToken == "" {
		return "", fmt.Errorf("openbao: auth/approle/login answered without a client token (%s)", strings.TrimSpace(string(body)))
	}
	return payload.Auth.ClientToken, nil
}

type CertRequest struct {
	CommonName string
	AltNames   []string
	TTL        string

	ExcludeCNFromSANs bool

	NotAfter string
}

type Cert struct {
	Certificate string
	PrivateKey  string
	IssuingCA   string
	Serial      string
}

type PKIRole struct {
	AllowedDomains  []string
	AllowSubdomains bool
	MaxTTL          string
}

func (c *Client) IssueCert(ctx context.Context, pkiPath, role string, req CertRequest) (Cert, error) {
	body := map[string]any{"common_name": req.CommonName}
	if len(req.AltNames) > 0 {
		body["alt_names"] = strings.Join(req.AltNames, ",")
	}
	if req.TTL != "" {
		body["ttl"] = req.TTL
	}
	if req.ExcludeCNFromSANs {
		body["exclude_cn_from_sans"] = true
	}
	if req.NotAfter != "" {
		body["not_after"] = req.NotAfter
	}
	raw, err := c.do(ctx, http.MethodPost, strings.Trim(pkiPath, "/")+"/issue/"+strings.Trim(role, "/"), body)
	if err != nil {
		return Cert{}, err
	}
	return parseCert(raw)
}

func (c *Client) ConfigurePKIURLs(ctx context.Context, pkiPath, issuingCertificates, crlDistributionPoints string) error {
	_, err := c.do(ctx, http.MethodPost, strings.Trim(pkiPath, "/")+"/config/urls", map[string]any{
		"issuing_certificates":    issuingCertificates,
		"crl_distribution_points": crlDistributionPoints,
	})
	return err
}

func (c *Client) GeneratePKIRoot(ctx context.Context, pkiPath, commonName, ttl string) (Cert, error) {
	body := map[string]any{"common_name": commonName}
	if ttl != "" {
		body["ttl"] = ttl
	}
	raw, err := c.do(ctx, http.MethodPost, strings.Trim(pkiPath, "/")+"/root/generate/internal", body)
	if err != nil {
		return Cert{}, err
	}
	return parseCert(raw)
}

func (c *Client) WritePKIRole(ctx context.Context, pkiPath, role string, cfg PKIRole) error {
	body := map[string]any{
		"allowed_domains":  cfg.AllowedDomains,
		"allow_subdomains": cfg.AllowSubdomains,
	}
	if cfg.MaxTTL != "" {
		body["max_ttl"] = cfg.MaxTTL
	}
	_, err := c.do(ctx, http.MethodPost, strings.Trim(pkiPath, "/")+"/roles/"+strings.Trim(role, "/"), body)
	return err
}

func parseCert(raw json.RawMessage) (Cert, error) {
	var payload struct {
		Certificate  string `json:"certificate"`
		PrivateKey   string `json:"private_key"`
		IssuingCA    string `json:"issuing_ca"`
		SerialNumber string `json:"serial_number"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Cert{}, fmt.Errorf("openbao: a PKI response was not JSON: %v (%s)", err, strings.TrimSpace(string(raw)))
	}
	return Cert{
		Certificate: payload.Certificate,
		PrivateKey:  payload.PrivateKey,
		IssuingCA:   payload.IssuingCA,
		Serial:      payload.SerialNumber,
	}, nil
}

// ponytail: one request per call, no retry or reuse policy; OpenBao returns 503 while sealed and a caller that needs resilience can wrap this client with bounded retry and its own backoff.
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
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: encode %s /v1/%s: %v", ErrTransport, method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build %s /v1/%s: %v", ErrTransport, method, path, err)
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s /v1/%s: %v", ErrTransport, method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read %s /v1/%s: %v", ErrTransport, method, path, err)
	}
	return resp.StatusCode, data, nil
}

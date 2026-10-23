package openbaoclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	openbao "github.com/openbao/openbao/api/v2"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

var (
	ErrNotFound = errors.New("openbao: not configured (HTTP 404)")

	ErrForbidden = errors.New("openbao: denied by policy (HTTP 403)")

	ErrTransport = errors.New("openbao: transport failure")

	ErrNoCA = errors.New("openbao: the supplied CA carries no CERTIFICATE PEM block")
)

const emptyMountData = "data from server response is empty"

type ResponseError struct {
	Method string

	Path string

	Status int

	Body string
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

type Options struct {
	Address string

	Token string

	CACert []byte

	ClientCert []byte

	ClientKey []byte

	Timeout time.Duration
}

type Client struct {
	client *openbao.Client
}

var _ vault.PKIBackend = (*Client)(nil)

func New(opts Options) (*Client, error) {
	if opts.Address == "" {
		return nil, errors.New("openbao: an address is required")
	}
	config := openbao.DefaultConfig()
	config.Address = opts.Address
	config.AgentAddress = ""
	config.SRVLookup = false
	config.Limiter = nil
	config.MaxRetries = 0
	config.Timeout = 30 * time.Second
	if opts.Timeout > 0 {
		config.Timeout = opts.Timeout
	}
	transport, ok := config.HttpClient.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("openbao: the client carries no HTTP transport")
	}
	transport.Proxy = nil
	tlsConfig := transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
		transport.TLSClientConfig = tlsConfig
	}
	tlsConfig.InsecureSkipVerify = false
	tlsConfig.ServerName = ""
	tlsConfig.GetClientCertificate = nil
	tlsConfig.RootCAs = nil
	if tlsConfig.MinVersion < tls.VersionTLS12 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	if len(opts.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CACert) {
			return nil, ErrNoCA
		}
		tlsConfig.RootCAs = pool
	}
	tlsConfig.Certificates = nil
	if len(opts.ClientCert) > 0 || len(opts.ClientKey) > 0 {
		cert, err := tls.X509KeyPair(opts.ClientCert, opts.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("openbao: the client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	client, err := openbao.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("openbao: %w", err)
	}
	client.SetToken(opts.Token)
	client.ClearNamespace()
	return &Client{client: client}, nil
}

func (c *Client) scoped(namespace string) *openbao.Client {
	return c.client.WithNamespace(namespace)
}

func (c *Client) Health(ctx context.Context) (vault.Health, error) {
	health, err := c.client.Sys().HealthWithContext(ctx)
	if err != nil {
		return vault.Health{}, translate(err, http.MethodGet, "sys/health")
	}
	return vault.Health{
		Initialized: health.Initialized,
		Sealed:      health.Sealed,
		Standby:     health.Standby,
		Version:     health.Version,
	}, nil
}

func (c *Client) EnsureNamespace(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	exists, err := c.NamespaceExists(ctx, path)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return c.write(ctx, "", "sys/namespaces/"+path, map[string]any{})
}

func (c *Client) NamespaceExists(ctx context.Context, path string) (bool, error) {
	response, err := c.readRaw(ctx, "", "sys/namespaces/"+path)
	if err == nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (c *Client) DeleteNamespace(ctx context.Context, path string) error {
	err := c.delete(ctx, "", "sys/namespaces/"+path)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) EnsureMount(ctx context.Context, namespace, path, kind string) error {
	mounts, err := c.scoped(namespace).Sys().ListMountsWithContext(ctx)
	if err != nil {
		if err.Error() != emptyMountData {
			return translate(err, http.MethodGet, "sys/mounts")
		}
		mounts = map[string]*openbao.MountOutput{}
	}
	key := strings.Trim(path, "/") + "/"
	if info, ok := mounts[key]; ok {
		if info.Type != kind {
			return fmt.Errorf("openbao: %s in namespace %q is mounted as %q, not %q, and this will not replace a mount in use",
				key, namespace, info.Type, kind)
		}
		return nil
	}
	if err := c.scoped(namespace).Sys().MountWithContext(ctx, strings.Trim(path, "/"), &openbao.MountInput{Type: kind}); err != nil {
		return translate(err, http.MethodPost, "sys/mounts/"+strings.Trim(path, "/"))
	}
	return nil
}

func (c *Client) GenerateRoot(ctx context.Context, namespace, mount, commonName, ttl string) (vault.RootCA, error) {
	body := map[string]any{"common_name": commonName}
	if ttl != "" {
		body["ttl"] = ttl
	}
	data, err := c.data(ctx, namespace, strings.Trim(mount, "/")+"/root/generate/internal", body)
	if err != nil {
		return vault.RootCA{}, err
	}
	out := vault.RootCA{Certificate: stringField(data, "certificate"), Serial: stringField(data, "serial_number")}
	if out.Serial == "" {
		out.Serial = serialOfPEM(out.Certificate)
	}
	return out, nil
}

func (c *Client) CASerial(ctx context.Context, namespace, mount string) (string, error) {
	data, err := c.read(ctx, namespace, strings.Trim(mount, "/")+"/cert/ca")
	if err != nil {
		var response *ResponseError
		if errors.Is(err, ErrNotFound) ||
			(errors.As(err, &response) && response.Status == http.StatusBadRequest &&
				strings.Contains(response.Body, "no default issuer")) {
			return "", fmt.Errorf("%w: %w: no CA at %s", vault.ErrNoAuthority, ErrNotFound, mount)
		}
		return "", err
	}
	if data == nil {
		return "", fmt.Errorf("%w: %w: no CA at %s", vault.ErrNoAuthority, ErrNotFound, mount)
	}
	return serialOfPEM(stringField(data, "certificate")), nil
}

func (c *Client) CAChain(ctx context.Context, namespace, mount string) (string, error) {
	response, err := c.readRaw(ctx, namespace, strings.Trim(mount, "/")+"/ca_chain")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("%w: read the chain for %s: %v", ErrTransport, mount, err)
	}
	return strings.TrimSpace(string(body)), nil
}

func (c *Client) GenerateIntermediate(ctx context.Context, namespace, mount, commonName string) (vault.IntermediateCSR, error) {
	data, err := c.data(ctx, namespace, strings.Trim(mount, "/")+"/intermediate/generate/internal", map[string]any{
		"common_name": commonName,
		"key_type":    "ec",
		"key_bits":    256,
	})
	if err != nil {
		return vault.IntermediateCSR{}, err
	}
	return vault.IntermediateCSR{CSR: stringField(data, "csr"), PrivateKey: stringField(data, "private_key")}, nil
}

func (c *Client) SignIntermediate(ctx context.Context, rootNamespace, mount, csr, commonName, ttl string) (string, error) {
	body := map[string]any{"csr": csr, "common_name": commonName}
	if ttl != "" {
		body["ttl"] = ttl
	}
	data, err := c.data(ctx, rootNamespace, strings.Trim(mount, "/")+"/root/sign-intermediate", body)
	if err != nil {
		return "", err
	}
	var chain strings.Builder
	chain.WriteString(stringField(data, "certificate"))
	for _, entry := range stringList(data, "ca_chain") {
		chain.WriteString("\n" + entry)
	}
	return chain.String(), nil
}

func (c *Client) SetSignedIntermediate(ctx context.Context, namespace, mount, chain string) error {
	return c.write(ctx, namespace, strings.Trim(mount, "/")+"/intermediate/set-signed", map[string]any{
		"certificate": chain,
	})
}

func (c *Client) WriteRole(ctx context.Context, namespace, mount, name string, role vault.Role) error {
	body := map[string]any{
		"allow_subdomains":   role.AllowSubdomains,
		"allow_bare_domains": role.AllowBareDomains,
		"enforce_hostnames":  role.EnforceHostnames,
		"allow_ip_sans":      true,
		"no_store":           role.NoStore,
	}
	if len(role.AllowedDomains) > 0 {
		body["allowed_domains"] = role.AllowedDomains
	}
	if role.KeyType != "" {
		body["key_type"] = role.KeyType
	}
	if role.KeyBits > 0 {
		body["key_bits"] = role.KeyBits
	}
	if role.MaxTTL != "" {
		body["max_ttl"] = role.MaxTTL
	}
	return c.write(ctx, namespace, strings.Trim(mount, "/")+"/roles/"+strings.Trim(name, "/"), body)
}

func (c *Client) Issue(ctx context.Context, namespace, mount, role string, req vault.CertRequest) (vault.Cert, error) {
	body := map[string]any{"common_name": req.CommonName}
	if len(req.AltNames) > 0 {
		body["alt_names"] = strings.Join(req.AltNames, ",")
	}
	if len(req.IPSANs) > 0 {
		body["ip_sans"] = strings.Join(req.IPSANs, ",")
	}
	if req.TTL != "" {
		body["ttl"] = req.TTL
	}
	data, err := c.data(ctx, namespace, strings.Trim(mount, "/")+"/issue/"+strings.Trim(role, "/"), body)
	if err != nil {
		return vault.Cert{}, err
	}
	return vault.Cert{
		Certificate: stringField(data, "certificate"),
		PrivateKey:  stringField(data, "private_key"),
		IssuingCA:   stringField(data, "issuing_ca"),
		CAChain:     stringList(data, "ca_chain"),
		Serial:      stringField(data, "serial_number"),
	}, nil
}

func (c *Client) write(ctx context.Context, namespace, path string, body map[string]any) error {
	_, err := c.data(ctx, namespace, path, body)
	return err
}

func (c *Client) delete(ctx context.Context, namespace, path string) error {
	if _, err := c.scoped(namespace).Logical().DeleteWithContext(ctx, path); err != nil {
		return translate(err, http.MethodDelete, path)
	}
	return nil
}

func (c *Client) read(ctx context.Context, namespace, path string) (map[string]any, error) {
	secret, err := c.scoped(namespace).Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, translate(err, http.MethodGet, path)
	}
	if secret == nil {
		return nil, nil
	}
	return secret.Data, nil
}

func (c *Client) data(ctx context.Context, namespace, path string, body map[string]any) (map[string]any, error) {
	secret, err := c.scoped(namespace).Logical().WriteWithContext(ctx, path, body)
	if err != nil {
		return nil, translate(err, http.MethodPost, path)
	}
	if secret == nil {
		return nil, nil
	}
	return secret.Data, nil
}

func (c *Client) readRaw(ctx context.Context, namespace, path string) (*openbao.Response, error) {
	response, err := c.scoped(namespace).Logical().ReadRawWithContext(ctx, strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, translate(err, http.MethodGet, path)
	}
	return response, nil
}

func translate(err error, method, path string) error {
	if err == nil {
		return nil
	}
	var responseError *openbao.ResponseError
	if errors.As(err, &responseError) {
		return &ResponseError{
			Method: method,
			Path:   path,
			Status: responseError.StatusCode,
			Body:   strings.Join(responseError.Errors, "; "),
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s /v1/%s: %v", ErrTransport, method, path, err)
}

func serialOfPEM(body string) string {
	block, _ := pem.Decode([]byte(body))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	hex := strings.ToUpper(cert.SerialNumber.Text(16))
	if len(hex)%2 == 1 {
		hex = "0" + hex
	}
	pairs := make([]string, 0, len(hex)/2)
	for i := 0; i < len(hex); i += 2 {
		pairs = append(pairs, hex[i:i+2])
	}
	return strings.Join(pairs, ":")
}

func stringField(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

func stringList(data map[string]any, key string) []string {
	raw, ok := data[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if value, ok := entry.(string); ok {
			out = append(out, value)
		}
	}
	return out
}

package openbao

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

type CertRequest struct {
	CommonName string
	AltNames   []string
	IPSANs     []string
	TTL        string

	ExcludeCNFromSANs bool

	NotAfter string
}

type Cert struct {
	Certificate string
	PrivateKey  string
	IssuingCA   string
	CAChain     []string
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
	if len(req.IPSANs) > 0 {
		body["ip_sans"] = strings.Join(req.IPSANs, ",")
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

func (c *Client) CAPEM(ctx context.Context, pkiMount string) (string, error) {
	return c.rawPEM(ctx, strings.Trim(pkiMount, "/")+"/ca/pem")
}

func (c *Client) CAChainPEM(ctx context.Context, pkiMount string) (string, error) {
	return c.rawPEM(ctx, strings.Trim(pkiMount, "/")+"/ca_chain")
}

func (c *Client) rawPEM(ctx context.Context, path string) (string, error) {
	status, body, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", &ResponseError{Method: http.MethodGet, Path: path, Status: status, Body: strings.TrimSpace(string(body))}
	}
	out := strings.TrimSpace(string(body))
	if out == "" {
		return "", fmt.Errorf("openbao: %s holds no certificate", path)
	}
	return out + "\n", nil
}

func parseCert(raw json.RawMessage) (Cert, error) {
	var payload struct {
		Certificate  string   `json:"certificate"`
		PrivateKey   string   `json:"private_key"`
		IssuingCA    string   `json:"issuing_ca"`
		CAChain      []string `json:"ca_chain"`
		SerialNumber string   `json:"serial_number"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Cert{}, fmt.Errorf("openbao: a PKI response was not JSON: %v", err)
	}
	return Cert{Certificate: payload.Certificate, PrivateKey: payload.PrivateKey, IssuingCA: payload.IssuingCA,
		CAChain: payload.CAChain, Serial: payload.SerialNumber}, nil
}

func (c *Client) Namespace(namespace string) *Client {
	out := c.with(c.token, c.renews)
	if out.api != nil {
		out.api = out.api.WithNamespace(namespace)
	}
	return out
}

type Backend struct {
	Client *Client
}

var _ vault.PKIBackend = Backend{}

func (b Backend) in(namespace string) *Client {
	if namespace == "" {
		return b.Client
	}
	return b.Client.Namespace(namespace)
}

func (b Backend) EnsureNamespace(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	_, err := b.Client.Read(ctx, "sys/namespaces/"+path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return b.Client.Write(ctx, "sys/namespaces/"+path, map[string]any{})
}

func (b Backend) DeleteNamespace(ctx context.Context, path string) error {
	return b.Client.Delete(ctx, "sys/namespaces/"+path)
}

func (b Backend) EnsureMount(ctx context.Context, namespace, path, kind string) error {
	c := b.in(namespace)
	mounts, err := c.SecretMounts(ctx)
	if err != nil {
		return err
	}
	key := strings.Trim(path, "/")
	if info, ok := mounts[key]; ok {
		if info.Type != kind {
			return fmt.Errorf("openbao: %s in namespace %q is mounted as %q, not %q, and this will not replace a mount in use",
				key, namespace, info.Type, kind)
		}
		return nil
	}
	return c.enableMount(ctx, key, map[string]any{"type": kind})
}

func (b Backend) GenerateRoot(ctx context.Context, namespace, mount, commonName, ttl string) (vault.RootCA, error) {
	cert, err := b.in(namespace).GeneratePKIRoot(ctx, mount, commonName, ttl)
	if err != nil {
		return vault.RootCA{}, err
	}
	out := vault.RootCA{Certificate: cert.Certificate, Serial: cert.Serial}
	if out.Serial == "" {
		out.Serial = serialOfPEM(out.Certificate)
	}
	return out, nil
}

func (b Backend) CASerial(ctx context.Context, namespace, mount string) (string, error) {
	data, err := b.in(namespace).Read(ctx, strings.Trim(mount, "/")+"/cert/ca")
	if err != nil {
		var response *ResponseError
		if errors.Is(err, ErrNotFound) ||
			(errors.As(err, &response) && response.Status == http.StatusBadRequest && strings.Contains(response.Body, "no default issuer")) {
			return "", fmt.Errorf("%w: %w: no CA at %s", vault.ErrNoAuthority, ErrNotFound, mount)
		}
		return "", err
	}
	certificate, _ := data["certificate"].(string)
	if certificate == "" {
		return "", fmt.Errorf("%w: %w: no CA at %s", vault.ErrNoAuthority, ErrNotFound, mount)
	}
	return serialOfPEM(certificate), nil
}

func (b Backend) CAChain(ctx context.Context, namespace, mount string) (string, error) {
	chain, err := b.in(namespace).CAChainPEM(ctx, mount)
	return strings.TrimSpace(chain), err
}

func (b Backend) GenerateIntermediate(ctx context.Context, namespace, mount, commonName string) (vault.IntermediateCSR, error) {
	data, err := b.in(namespace).WriteData(ctx, strings.Trim(mount, "/")+"/intermediate/generate/internal", map[string]any{
		"common_name": commonName,
		"key_type":    "ec",
		"key_bits":    256,
	})
	if err != nil {
		return vault.IntermediateCSR{}, err
	}
	return vault.IntermediateCSR{CSR: stringField(data, "csr"), PrivateKey: stringField(data, "private_key")}, nil
}

func (b Backend) SignIntermediate(ctx context.Context, rootNamespace, mount, csr, commonName, ttl string) (string, error) {
	body := map[string]any{"csr": csr, "common_name": commonName}
	if ttl != "" {
		body["ttl"] = ttl
	}
	data, err := b.in(rootNamespace).WriteData(ctx, strings.Trim(mount, "/")+"/root/sign-intermediate", body)
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

func (b Backend) SetSignedIntermediate(ctx context.Context, namespace, mount, chain string) error {
	return b.in(namespace).Write(ctx, strings.Trim(mount, "/")+"/intermediate/set-signed", map[string]any{"certificate": chain})
}

func (b Backend) WriteRole(ctx context.Context, namespace, mount, name string, role vault.Role) error {
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
	return b.in(namespace).Write(ctx, strings.Trim(mount, "/")+"/roles/"+strings.Trim(name, "/"), body)
}

func (b Backend) Issue(ctx context.Context, namespace, mount, role string, req vault.CertRequest) (vault.Cert, error) {
	cert, err := b.in(namespace).IssueCert(ctx, mount, role, CertRequest{CommonName: req.CommonName, AltNames: req.AltNames,
		IPSANs: req.IPSANs, TTL: req.TTL})
	if err != nil {
		return vault.Cert{}, err
	}
	return vault.Cert{Certificate: cert.Certificate, PrivateKey: cert.PrivateKey, IssuingCA: cert.IssuingCA,
		CAChain: cert.CAChain, Serial: cert.Serial}, nil
}

func CAPoolFetch(pki vault.PKI, mount string, lifetime time.Duration) lease.Fetch[*x509.CertPool] {
	return func(ctx context.Context) (*x509.CertPool, time.Time, error) {
		chain, err := pki.CAChain(ctx, mount)
		if err != nil {
			return nil, time.Time{}, err
		}
		pool, err := CertPoolFromPEM([]byte(chain))
		if err != nil {
			return nil, time.Time{}, err
		}
		return pool, time.Now().Add(lifetime), nil
	}
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

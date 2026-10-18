package vaultcert

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	PKIMount    = "pki"
	PKIRoleNode = "node-client"

	CertLoginPath = "auth/cert/login"

	NodeCertFile = "vault-client.crt"
	NodeKeyFile  = "vault-client.key"
	NodeCAFile   = "vault-ca.crt"
	NodeMetaFile = "vault-client.json"

	NodeCertTTL  = 24 * time.Hour
	NodeMaxTTL   = 168 * time.Hour
	NodeTokenTTL = "15m"
	NodeTokenMax = "1h"

	nodeFileMode = 0o600
)

var ErrNoVaultClientCA = errors.New(
	"vaultcert: the cluster vault holds no imported client CA at pki/ca/pem, so a certificate it " +
		"issues would chain to nothing and the CERT auth role would trust no issuer. Import the " +
		"cluster's client-ca into the cluster vault before minting, which is what custody does")

var ErrCertLoginRefused = errors.New("vaultcert: the CERT login was refused")

type ClientMetadata struct {
	Identity string `json:"identity"`

	Role string `json:"role"`

	VaultAddr string `json:"vaultAddr"`
}

type ClientCertificate struct {
	Metadata ClientMetadata

	CertPEM []byte

	KeyPEM []byte

	ServerCAPEM []byte

	Serial string
}

func (c ClientCertificate) WriteTo(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("vaultcert: creating the certificate directory %s: %w", dir, err)
	}
	meta, err := json.Marshal(c.Metadata)
	if err != nil {
		return fmt.Errorf("vaultcert: encoding the certificate metadata: %w", err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{NodeCertFile, c.CertPEM},
		{NodeKeyFile, c.KeyPEM},
		{NodeCAFile, c.ServerCAPEM},
		{NodeMetaFile, meta},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, nodeFileMode); err != nil {
			return fmt.Errorf("vaultcert: writing %s: %w", filepath.Join(dir, f.name), err)
		}
	}
	return nil
}

func Load(dir string) (ClientCertificate, error) {
	var out ClientCertificate
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{
		{NodeCertFile, &out.CertPEM},
		{NodeKeyFile, &out.KeyPEM},
		{NodeCAFile, &out.ServerCAPEM},
	} {
		raw, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return ClientCertificate{}, fmt.Errorf("vaultcert: reading %s: %w", filepath.Join(dir, f.name), err)
		}
		*f.dst = raw
	}
	meta, err := os.ReadFile(filepath.Join(dir, NodeMetaFile))
	if err != nil {
		return ClientCertificate{}, fmt.Errorf("vaultcert: reading %s: %w", filepath.Join(dir, NodeMetaFile), err)
	}
	if err := json.Unmarshal(meta, &out.Metadata); err != nil {
		return ClientCertificate{}, fmt.Errorf("vaultcert: the metadata at %s is not JSON: %w", filepath.Join(dir, NodeMetaFile), err)
	}
	return out, nil
}

func IssueESOCertificate(ctx context.Context, client *openbao.Client, vaultAddr string, serverCAPEM []byte) (ClientCertificate, error) {
	if len(serverCAPEM) == 0 {
		return ClientCertificate{}, errors.New(
			"vaultcert: no vault server CA was given, so the certificate would carry no way to " +
				"verify the vault it dials and every read would need server verification switched off")
	}
	issued, err := client.IssueCert(ctx, PKIMount, PKIRoleNode, openbao.CertRequest{
		CommonName: vaultlayout.RoleESO,
		TTL:        vaultTTLString(NodeCertTTL),
	})
	if err != nil {
		return ClientCertificate{}, fmt.Errorf("vaultcert: issuing the certificate for %s: %w", vaultlayout.RoleESO, err)
	}
	if issued.Certificate == "" || issued.PrivateKey == "" {
		return ClientCertificate{}, fmt.Errorf("vaultcert: the vault issued an incomplete certificate for %s", vaultlayout.RoleESO)
	}
	leaf, err := parseLeaf([]byte(issued.Certificate))
	if err != nil {
		return ClientCertificate{}, err
	}
	if leaf.Subject.CommonName != vaultlayout.RoleESO {
		return ClientCertificate{}, fmt.Errorf("vaultcert: the vault issued a certificate whose common name is %q, not %q",
			leaf.Subject.CommonName, vaultlayout.RoleESO)
	}
	return ClientCertificate{
		Metadata:    ClientMetadata{Identity: vaultlayout.RoleESO, Role: vaultlayout.RoleESO, VaultAddr: vaultAddr},
		CertPEM:     []byte(issued.Certificate),
		KeyPEM:      []byte(issued.PrivateKey),
		ServerCAPEM: serverCAPEM,
		Serial:      issued.Serial,
	}, nil
}

func (c ClientCertificate) Login(ctx context.Context) (string, error) {
	hc, err := c.tlsClient()
	if err != nil {
		return "", err
	}
	status, body, err := newRawClient(c.Metadata.VaultAddr, "", hc).do(ctx, http.MethodPost, CertLoginPath, map[string]any{
		"name": c.Metadata.Role,
	})
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", fmt.Errorf("%w: %s -> HTTP %d: %s", ErrCertLoginRefused, c.Metadata.VaultAddr, status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("vaultcert: the CERT login answered a body that is not JSON: %v (%s)", err, strings.TrimSpace(string(body)))
	}
	if payload.Auth.ClientToken == "" {
		return "", fmt.Errorf("vaultcert: the CERT login answered without a client token (%s)", strings.TrimSpace(string(body)))
	}
	return payload.Auth.ClientToken, nil
}

func (c ClientCertificate) ReadKV(ctx context.Context, path string) (map[string]any, error) {
	token, err := c.Login(ctx)
	if err != nil {
		return nil, err
	}
	hc, err := c.tlsClient()
	if err != nil {
		return nil, err
	}
	status, body, err := newRawClient(c.Metadata.VaultAddr, token, hc).do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, &openbao.ResponseError{Method: http.MethodGet, Path: path, Status: status, Body: strings.TrimSpace(string(body))}
	}
	var env struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("vaultcert: GET %s/v1/%s was not JSON: %v", c.Metadata.VaultAddr, path, err)
	}
	return env.Data.Data, nil
}

func (c ClientCertificate) FetchOwnSecret(ctx context.Context) (map[string]any, error) {
	if c.Metadata.Identity == "" {
		return nil, errors.New("vaultcert: this certificate carries no identity, so it names no per-node path to read")
	}
	return c.ReadKV(ctx, vaultlayout.NodeSecretPath(c.Metadata.Identity))
}

func (c ClientCertificate) tlsClient() (*http.Client, error) {
	pair, err := tls.X509KeyPair(c.CertPEM, c.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("vaultcert: the certificate and key are not a pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.ServerCAPEM) {
		return nil, errors.New("vaultcert: the vault server CA carries no CERTIFICATE PEM block")
	}
	return &http.Client{Transport: &http.Transport{IdleConnTimeout: openbao.IdleConnTimeout, TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
	}}}, nil
}

type rawClient struct {
	addr  string
	token string
	http  *http.Client
}

func newRawClient(addr, token string, hc *http.Client) *rawClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &rawClient{addr: strings.TrimRight(addr, "/"), token: token, http: hc}
}

func adminHTTPClient(pool *x509.CertPool) *http.Client {
	return &http.Client{Transport: &http.Transport{IdleConnTimeout: openbao.IdleConnTimeout, TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}}}
}

func (c *rawClient) do(ctx context.Context, method, path string, in any) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, nil, fmt.Errorf("vaultcert: encoding %s /v1/%s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+strings.Trim(path, "/"), body)
	if err != nil {
		return 0, nil, fmt.Errorf("vaultcert: building %s /v1/%s: %w", method, path, err)
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("vaultcert: %s /v1/%s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("vaultcert: reading %s /v1/%s: %w", method, path, err)
	}
	return resp.StatusCode, data, nil
}

func (c *rawClient) listData(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	status, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("vaultcert: GET /v1/%s -> HTTP %d: %s", path, status, strings.TrimSpace(string(body)))
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("vaultcert: GET /v1/%s was not JSON: %v", path, err)
	}
	return env.Data, nil
}

func vaultTTLString(d time.Duration) string {
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	certs, err := parseCertificates(certPEM)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, errors.New("vaultcert: the certificate holds no PEM block")
	}
	return certs[0], nil
}

func parseCertificates(certPEM []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for block, rest := pem.Decode(certPEM); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("vaultcert: a PEM block is not a certificate: %w", err)
		}
		out = append(out, cert)
	}
	return out, nil
}

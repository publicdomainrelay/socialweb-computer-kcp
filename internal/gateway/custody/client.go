package custody

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const custodyMaxBody = 1 << 20

type Endpoint struct {
	Address string

	CACert []byte

	ClientCert []byte

	ClientKey []byte

	CertMount string

	CertRole string

	Mount string

	Timeout time.Duration
}

func (e Endpoint) check() error {
	if e.Address == "" {
		return errors.New("gatewayapi: the custody endpoint has no address")
	}
	u, err := url.Parse(e.Address)
	if err != nil {
		return fmt.Errorf("gatewayapi: the custody address %q: %w", e.Address, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("gatewayapi: the custody address %q is not an absolute http or https URL", e.Address)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("gatewayapi: the custody address %q carries user info or a path", e.Address)
	}
	if u.Scheme == "http" && !LoopbackHost(u.Hostname()) {
		return fmt.Errorf("gatewayapi: the custody address %q is plain http on a non-loopback host", e.Address)
	}
	if e.Mount == "" {
		return errors.New("gatewayapi: the custody endpoint has no atproto mount")
	}
	return nil
}

type custodyEnvelope struct {
	Data json.RawMessage `json:"data"`

	Auth *struct {
		ClientToken string `json:"client_token"`

		LeaseDuration int64 `json:"lease_duration"`

		Renewable bool `json:"renewable"`

		Metadata map[string]string `json:"metadata"`

		Policies []string `json:"policies"`
	} `json:"auth"`

	Errors []string `json:"errors"`
}

type custodyError struct {
	Operation string

	Status int

	Message string
}

func (e *custodyError) Error() string {
	return fmt.Sprintf("gatewayapi: custody %s: HTTP %d: %s", e.Operation, e.Status, e.Message)
}

type custodyClient struct {
	endpoint Endpoint

	http *http.Client
}

func newCustodyClient(endpoint Endpoint) (*custodyClient, error) {
	if err := endpoint.check(); err != nil {
		return nil, err
	}
	tlsConfig, err := httpclient.TLSConfig(httpclient.TLSOptions{CAPEM: endpoint.CACert, CertPEM: endpoint.ClientCert, KeyPEM: endpoint.ClientKey})
	if errors.Is(err, httpclient.ErrNoCA) {
		return nil, errors.New("gatewayapi: the custody CA bundle holds no certificate")
	}
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the custody client certificate: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	timeout := endpoint.Timeout
	timeout = opt.Positive(timeout, DefaultTimeout)
	return &custodyClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

func (c *custodyClient) call(ctx context.Context, operation, method, path, token string, body any) (*custodyEnvelope, int, error) {
	status, raw, err := openbao.New(c.endpoint.Address, token, openbao.WithHTTPClient(c.http)).Raw(ctx, method, strings.TrimLeft(path, "/"), body)
	if err != nil {
		return nil, 0, fmt.Errorf("gatewayapi: custody %s: %w", operation, err)
	}
	if len(raw) > custodyMaxBody {
		return nil, 0, &custodyError{Operation: operation, Status: status, Message: "the answer is too large"}
	}
	var envelope custodyEnvelope
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, status, &custodyError{Operation: operation, Status: status, Message: "the answer is not JSON: " + truncate(string(raw), 200)}
		}
	}
	return &envelope, status, nil
}

func (c *custodyClient) expect(ctx context.Context, operation, method, path, token string, body any) (json.RawMessage, *custodyEnvelope, error) {
	envelope, status, err := c.call(ctx, operation, method, path, token, body)
	if err != nil {
		return nil, nil, err
	}
	if status < 200 || status > 299 {
		return nil, nil, &custodyError{Operation: operation, Status: status, Message: envelopeMessage(envelope)}
	}
	return envelope.Data, envelope, nil
}

func envelopeMessage(envelope *custodyEnvelope) string {
	if envelope == nil {
		return "no answer"
	}
	if len(envelope.Errors) > 0 {
		return strings.Join(envelope.Errors, "; ")
	}
	if len(envelope.Data) > 0 {
		return truncate(string(envelope.Data), 300)
	}
	return "no error detail"
}

func truncate(raw string, limit int) string {
	raw = strings.TrimSpace(raw)
	if len(raw) <= limit {
		return raw
	}
	return raw[:limit] + "..."
}

func (c *custodyClient) clientMetadata(ctx context.Context) (json.RawMessage, error) {
	path := "auth/" + strings.Trim(c.endpoint.Mount, "/") + "/client-metadata"
	data, _, err := c.expect(ctx, "client metadata", http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("gatewayapi: decoding the custody client metadata: %w", err)
	}
	if _, ok := document["client_id"]; !ok {
		return nil, errors.New("gatewayapi: the custody client metadata is empty")
	}
	return append(json.RawMessage(nil), data...), nil
}

func (c *custodyClient) start(ctx context.Context, role, identifier, redirectURI string) (authorizationURL, state string, err error) {
	body := map[string]any{"role": role, "redirect_uri": redirectURI}
	if strings.HasPrefix(identifier, "did:") {
		body["did"] = identifier
	} else {
		body["handle"] = identifier
	}
	path := "auth/" + strings.Trim(c.endpoint.Mount, "/") + "/login/start"
	data, _, err := c.expect(ctx, "start", http.MethodPost, path, "", body)
	if err != nil {
		return "", "", err
	}
	var payload struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", "", fmt.Errorf("gatewayapi: decoding the custody start: %w", err)
	}
	if payload.AuthorizationURL == "" || payload.State == "" {
		return "", "", errors.New("gatewayapi: the custody start returned no authorization URL or state")
	}
	return payload.AuthorizationURL, payload.State, nil
}

type custodyLogin struct {
	DID string

	Handle string

	PDS string

	Token string

	LeaseSeconds int64
}

func (c *custodyClient) complete(ctx context.Context, state, code, iss string) (custodyLogin, error) {
	body := map[string]any{"state": state, "code": code}
	if iss != "" {
		body["iss"] = iss
	}
	path := "auth/" + strings.Trim(c.endpoint.Mount, "/") + "/login"
	_, envelope, err := c.expect(ctx, "complete", http.MethodPost, path, "", body)
	if err != nil {
		return custodyLogin{}, err
	}
	if envelope.Auth == nil || envelope.Auth.ClientToken == "" {
		return custodyLogin{}, errors.New("gatewayapi: the custody complete returned no client token")
	}
	login := custodyLogin{
		DID:          envelope.Auth.Metadata["did"],
		Handle:       envelope.Auth.Metadata["handle"],
		PDS:          envelope.Auth.Metadata["pds"],
		Token:        envelope.Auth.ClientToken,
		LeaseSeconds: envelope.Auth.LeaseDuration,
	}
	if login.DID == "" {
		return custodyLogin{}, errors.New("gatewayapi: the custody complete returned no did")
	}
	return login, nil
}

type custodyIdentity struct {
	DID string

	Handle string

	PDS string

	Policies []string

	TTLSeconds int64

	Renewable bool
}

func (c *custodyClient) identify(ctx context.Context, token string) (*custodyIdentity, error) {
	envelope, status, err := c.call(ctx, "identify", http.MethodGet, "auth/token/lookup-self", token, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden {
		return nil, nil
	}
	if status < 200 || status > 299 {
		return nil, &custodyError{Operation: "identify", Status: status, Message: envelopeMessage(envelope)}
	}
	if envelope == nil || len(envelope.Data) == 0 {
		return nil, nil
	}
	var payload struct {
		Meta      map[string]string `json:"meta"`
		Policies  []string          `json:"policies"`
		TTL       float64           `json:"ttl"`
		Renewable bool              `json:"renewable"`
	}
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		return nil, fmt.Errorf("gatewayapi: decoding the custody identity: %w", err)
	}
	if payload.Meta["did"] == "" {
		return nil, nil
	}
	return &custodyIdentity{
		DID:        payload.Meta["did"],
		Handle:     payload.Meta["handle"],
		PDS:        payload.Meta["pds"],
		Policies:   payload.Policies,
		TTLSeconds: int64(payload.TTL),
		Renewable:  payload.Renewable,
	}, nil
}

func (c *custodyClient) renew(ctx context.Context, token string, increment time.Duration) (int64, error) {
	body := map[string]any{}
	if increment > 0 {
		body["increment"] = fmt.Sprintf("%ds", int64(increment/time.Second))
	}
	_, envelope, err := c.expect(ctx, "renew", http.MethodPost, "auth/token/renew-self", token, body)
	if err != nil {
		return 0, err
	}
	if envelope.Auth == nil {
		return 0, nil
	}
	return envelope.Auth.LeaseDuration, nil
}

func (c *custodyClient) revokeToken(ctx context.Context, token string) error {
	_, status, err := c.call(ctx, "revoke token", http.MethodPost, "auth/token/revoke-self", token, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusForbidden {
		return nil
	}
	return &custodyError{Operation: "revoke token", Status: status, Message: "the token was not revoked"}
}

func (c *custodyClient) signOut(ctx context.Context, token, did string) (bool, string, error) {
	path := c.sessionPath(did)
	envelope, status, err := c.call(ctx, "sign out", http.MethodDelete, path, token, nil)
	if err != nil {
		return false, "", err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return false, "", &custodyError{Operation: "sign out", Status: status, Message: envelopeMessage(envelope)}
	}
	if envelope == nil || len(envelope.Data) == 0 {
		return false, "", nil
	}
	var payload struct {
		Revoked     bool   `json:"revoked"`
		RevokeError string `json:"revoke_error"`
	}
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		return false, "", fmt.Errorf("gatewayapi: decoding the custody sign out: %w", err)
	}
	return payload.Revoked, payload.RevokeError, nil
}

func (c *custodyClient) sessionPath(did string) string {
	return "auth/" + strings.Trim(c.endpoint.Mount, "/") + "/session/" + url.PathEscape(did)
}

type custodyProof struct {
	AccessToken string

	DPoP string
}

func (c *custodyClient) proof(ctx context.Context, token, did, method, target, nonce string) (custodyProof, error) {
	body := map[string]any{"htm": method, "htu": target}
	if nonce != "" {
		body["nonce"] = nonce
	}
	data, _, err := c.expect(ctx, "proof", http.MethodPost, c.sessionPath(did)+"/proof", token, body)
	if err != nil {
		return custodyProof{}, err
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		DPoPProof   string `json:"dpop_proof"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return custodyProof{}, fmt.Errorf("gatewayapi: decoding the custody proof: %w", err)
	}
	if payload.AccessToken == "" || payload.DPoPProof == "" {
		return custodyProof{}, errors.New("gatewayapi: the custody proof returned no token or DPoP proof")
	}
	return custodyProof{AccessToken: payload.AccessToken, DPoP: payload.DPoPProof}, nil
}

func LoopbackHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

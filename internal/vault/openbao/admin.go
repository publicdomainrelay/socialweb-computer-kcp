package openbao

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

type Health = vault.Health

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
}

func (c *Client) Init(ctx context.Context, shares, threshold int) (InitResult, *Client, error) {
	raw, err := c.topLevel(ctx, http.MethodPut, "sys/init", map[string]any{
		"secret_shares":    shares,
		"secret_threshold": threshold,
	})
	if err != nil {
		return InitResult{}, nil, err
	}
	var payload struct {
		Keys       []string `json:"keys"`
		KeysBase64 []string `json:"keys_base64"`
		RootToken  string   `json:"root_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return InitResult{}, nil, fmt.Errorf("openbao: sys/init returned a body that is not JSON: %v", err)
	}
	return InitResult{Keys: payload.Keys, KeysBase64: payload.KeysBase64}, c.WithToken(payload.RootToken), nil
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

func (c *Client) DeletePolicy(ctx context.Context, name string) error {
	return c.Delete(ctx, "sys/policies/acl/"+strings.Trim(name, "/"))
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
		Data map[string]any `json:"data"`
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("openbao: reading %q returned a body that is not JSON: %v", path, err)
	}
	return payload.Data, nil
}

func (c *Client) WriteData(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	raw, err := c.do(ctx, http.MethodPost, strings.Trim(path, "/"), body)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal(trimmed, &data); err != nil {
		return nil, fmt.Errorf("openbao: reading %q returned a body that is not JSON: %v", path, err)
	}
	return data, nil
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
		return "", fmt.Errorf("openbao: reading the role id for %q returned a body that is not JSON: %v", name, err)
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
		return "", fmt.Errorf("openbao: reading the secret id for %q returned a body that is not JSON: %v", name, err)
	}
	return payload.SecretID, nil
}

type Mount struct {
	Type          string         `json:"type"`
	Options       map[string]any `json:"options"`
	RunningSHA256 string         `json:"running_sha256"`
	Accessor      string         `json:"accessor"`
}

func (c *Client) SecretMounts(ctx context.Context) (map[string]Mount, error) {
	return c.mounts(ctx, "sys/mounts")
}

func (c *Client) AuthMounts(ctx context.Context) (map[string]Mount, error) {
	return c.mounts(ctx, "sys/auth")
}

func (c *Client) mounts(ctx context.Context, path string) (map[string]Mount, error) {
	raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]Mount{}
	if len(raw) == 0 {
		return out, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("openbao: GET /v1/%s returned a body that is not JSON: %v", path, err)
	}
	for name, entry := range entries {
		var m Mount
		if json.Unmarshal(entry, &m) != nil || m.Type == "" {
			continue
		}
		out[strings.Trim(name, "/")] = m
	}
	return out, nil
}

func (c *Client) DisableMount(ctx context.Context, path string) error {
	_, err := c.do(ctx, http.MethodDelete, "sys/mounts/"+strings.Trim(path, "/"), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) TuneMount(ctx context.Context, path string, body map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, "sys/mounts/"+strings.Trim(path, "/")+"/tune", body)
	return err
}

func (c *Client) EnableAuth(ctx context.Context, path, kind string) error {
	_, err := c.do(ctx, http.MethodPut, "sys/auth/"+strings.Trim(path, "/"), map[string]any{"type": kind})
	return err
}

func (c *Client) DisableAuth(ctx context.Context, path string) error {
	_, err := c.do(ctx, http.MethodDelete, "sys/auth/"+strings.Trim(path, "/"), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) Write(ctx context.Context, path string, body map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, strings.Trim(path, "/"), body)
	return err
}

func (c *Client) Read(ctx context.Context, path string) (map[string]any, error) {
	raw, err := c.do(ctx, http.MethodGet, strings.Trim(path, "/"), nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("openbao: reading %q returned data that is not a JSON object: %v", path, err)
	}
	return out, nil
}

func (c *Client) Delete(ctx context.Context, path string) error {
	_, err := c.do(ctx, http.MethodDelete, strings.Trim(path, "/"), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) List(ctx context.Context, path string) ([]string, error) {
	raw, err := c.do(ctx, "LIST", strings.Trim(path, "/"), nil)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Keys []string `json:"keys"`
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("openbao: listing %q returned a body that is not JSON: %v", path, err)
	}
	return payload.Keys, nil
}

type generateRootStatus struct {
	Nonce        string `json:"nonce"`
	Started      bool   `json:"started"`
	Complete     bool   `json:"complete"`
	OTP          string `json:"otp"`
	OTPLength    int    `json:"otp_length"`
	EncodedToken string `json:"encoded_token"`
}

var ErrGenerateRoot = errors.New("openbao: generating a root token from the unseal keys failed")

func (c *Client) GenerateRoot(ctx context.Context, keys []string, threshold int) (*Client, error) {
	if _, err := c.topLevel(ctx, http.MethodDelete, "sys/generate-root/attempt", nil); err != nil {
		return nil, fmt.Errorf("%w: cancelling a stale attempt: %w", ErrGenerateRoot, err)
	}
	raw, err := c.topLevel(ctx, http.MethodPut, "sys/generate-root/attempt", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("%w: starting: %w", ErrGenerateRoot, err)
	}
	var st generateRootStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%w: the attempt answer is not JSON: %v", ErrGenerateRoot, err)
	}
	if st.OTP == "" || st.Nonce == "" {
		return nil, fmt.Errorf("%w: the server returned no one-time pad", ErrGenerateRoot)
	}
	otp := st.OTP
	if len(keys) < threshold {
		return nil, fmt.Errorf("%w: %d keys for a threshold of %d", ErrGenerateRoot, len(keys), threshold)
	}
	for i := 0; i < threshold && !st.Complete; i++ {
		raw, err := c.topLevel(ctx, http.MethodPut, "sys/generate-root/update", map[string]any{"key": keys[i], "nonce": st.Nonce})
		if err != nil {
			return nil, fmt.Errorf("%w: key %d: %w", ErrGenerateRoot, i+1, err)
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("%w: the update answer is not JSON: %v", ErrGenerateRoot, err)
		}
	}
	if !st.Complete || st.EncodedToken == "" {
		return nil, fmt.Errorf("%w: not complete after %d keys", ErrGenerateRoot, threshold)
	}
	enc, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(st.EncodedToken, "="))
	if err != nil {
		return nil, fmt.Errorf("%w: decoding the encoded token: %v", ErrGenerateRoot, err)
	}
	if len(enc) != len(otp) {
		return nil, fmt.Errorf("%w: the encoded token is %d bytes and the pad %d", ErrGenerateRoot, len(enc), len(otp))
	}
	out := make([]byte, len(enc))
	for i := range enc {
		out[i] = enc[i] ^ otp[i]
	}
	return c.WithToken(string(out)), nil
}

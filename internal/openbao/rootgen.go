package openbao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (c *Client) WithToken(token string) *Client {
	return &Client{addr: c.addr, token: token, http: c.http, tls: c.tls}
}

func (c *Client) Token() string { return c.currentToken() }

func (c *Client) LoginCert(ctx context.Context, mount, role string) (string, error) {
	path := "auth/" + strings.Trim(mount, "/") + "/login"
	status, body, err := c.request(ctx, http.MethodPost, path, map[string]any{"name": role})
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", &ResponseError{Method: http.MethodPost, Path: path, Status: status, Body: strings.TrimSpace(string(body))}
	}
	var payload struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("openbao: %s returned a body that is not JSON: %v", path, err)
	}
	if payload.Auth.ClientToken == "" {
		return "", fmt.Errorf("openbao: %s answered without a client token", path)
	}
	return payload.Auth.ClientToken, nil
}

func (c *Client) RevokeSelf(ctx context.Context) error {
	_, err := c.topLevel(ctx, http.MethodPost, "auth/token/revoke-self", nil)
	return err
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

func (c *Client) GenerateRoot(ctx context.Context, keys []string, threshold int) (string, error) {
	if _, err := c.topLevel(ctx, http.MethodDelete, "sys/generate-root/attempt", nil); err != nil {
		return "", fmt.Errorf("%w: cancelling a stale attempt: %w", ErrGenerateRoot, err)
	}
	raw, err := c.topLevel(ctx, http.MethodPut, "sys/generate-root/attempt", map[string]any{})
	if err != nil {
		return "", fmt.Errorf("%w: starting: %w", ErrGenerateRoot, err)
	}
	var st generateRootStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("%w: the attempt answer is not JSON: %v", ErrGenerateRoot, err)
	}
	if st.OTP == "" || st.Nonce == "" {
		return "", fmt.Errorf("%w: the server returned no one-time pad", ErrGenerateRoot)
	}
	otp := st.OTP
	if len(keys) < threshold {
		return "", fmt.Errorf("%w: %d keys for a threshold of %d", ErrGenerateRoot, len(keys), threshold)
	}
	for i := 0; i < threshold && !st.Complete; i++ {
		raw, err := c.topLevel(ctx, http.MethodPut, "sys/generate-root/update", map[string]any{"key": keys[i], "nonce": st.Nonce})
		if err != nil {
			return "", fmt.Errorf("%w: key %d: %w", ErrGenerateRoot, i+1, err)
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return "", fmt.Errorf("%w: the update answer is not JSON: %v", ErrGenerateRoot, err)
		}
	}
	if !st.Complete || st.EncodedToken == "" {
		return "", fmt.Errorf("%w: not complete after %d keys", ErrGenerateRoot, threshold)
	}
	enc, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(st.EncodedToken, "="))
	if err != nil {
		return "", fmt.Errorf("%w: decoding the encoded token: %v", ErrGenerateRoot, err)
	}
	if len(enc) != len(otp) {
		return "", fmt.Errorf("%w: the encoded token is %d bytes and the pad %d", ErrGenerateRoot, len(enc), len(otp))
	}
	out := make([]byte, len(enc))
	for i := range enc {
		out[i] = enc[i] ^ otp[i]
	}
	return string(out), nil
}

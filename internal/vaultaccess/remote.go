package vaultaccess

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials/openbaosession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

type RemoteInspector interface {
	Authenticated(ctx context.Context) error

	MountType(ctx context.Context, path string) (string, bool, error)

	AuthMountType(ctx context.Context, path string) (string, bool, error)

	Capabilities(ctx context.Context, paths []string) (map[string][]string, error)
}

type loginResult struct {
	token string

	lease time.Duration

	renewable bool
}

type remoteSession struct {
	address string

	httpClient *http.Client

	opts []openbao.Option

	now func() time.Time

	login func(context.Context) (loginResult, error)

	mu sync.Mutex

	client *openbao.Client

	token string

	lease time.Duration

	renewable bool

	renewAt time.Time
}

var _ Session = (*remoteSession)(nil)

var _ CertificateIssuer = (*remoteSession)(nil)

var _ RemoteInspector = (*remoteSession)(nil)

func newRemoteSession(target Target, result loginResult, login func(context.Context) (loginResult, error), now func() time.Time) *remoteSession {
	if now == nil {
		now = time.Now
	}
	options := []openbao.Option{openbao.WithHTTPClient(target.HTTPClient)}
	session := &remoteSession{
		address:    target.URL,
		httpClient: target.HTTPClient,
		opts:       options,
		now:        now,
		login:      login,
	}
	session.apply(result)
	return session
}

func (s *remoteSession) apply(result loginResult) {
	result.token = cmp.Or(result.token, s.token)
	s.token = result.token
	s.lease = result.lease
	s.renewable = result.renewable
	s.client = openbao.New(s.address, s.token, s.opts...)
	switch {
	case result.lease <= 0:
		s.renewAt = time.Time{}
	case result.renewable:
		s.renewAt = s.now().Add(result.lease / 2)
	default:
		s.renewAt = s.now().Add(result.lease)
	}
}

func (s *remoteSession) clientFor(ctx context.Context) (*openbao.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil, errors.New("vaultaccess: the remote session has no client")
	}
	if s.lease <= 0 || s.now().Before(s.renewAt) {
		return s.client, nil
	}
	if !s.renewable {
		if s.login == nil {
			return nil, fmt.Errorf("vaultaccess: the static token of %s expired and has no renewal source; replace its tokenRef", s.address)
		}
		result, err := s.login(ctx)
		if err != nil {
			return nil, fmt.Errorf("vaultaccess: logging in again: %w", err)
		}
		s.apply(result)
		return s.client, nil
	}
	result, err := renewToken(ctx, s.httpClient, s.address, s.token)
	if err != nil {
		if s.login == nil {
			return nil, fmt.Errorf("vaultaccess: renewing the token: %w", err)
		}
		result, err = s.login(ctx)
		if err != nil {
			return nil, fmt.Errorf("vaultaccess: logging in again: %w", err)
		}
	}
	s.apply(result)
	return s.client, nil
}

func (s *remoteSession) ReadKV(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	kvPath, err := kvDataPath(mount, path)
	if err != nil {
		return nil, err
	}
	client, err := s.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	if version <= 0 {
		return client.GetKV(ctx, kvPath)
	}
	raw, err := client.Read(ctx, fmt.Sprintf("%s?version=%d", kvPath, version))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	data, ok := raw["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("vaultaccess: %s at version %d did not return a KV v2 data object", kvPath, version)
	}
	return data, nil
}

func (s *remoteSession) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	kvPath, err := kvDataPath(mount, path)
	if err != nil {
		return err
	}
	client, err := s.clientFor(ctx)
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	return client.PutKV(ctx, kvPath, data)
}

func (s *remoteSession) DeleteKV(ctx context.Context, mount, path string) error {
	kvPath, err := kvMetadataPath(mount, path)
	if err != nil {
		return err
	}
	client, err := s.clientFor(ctx)
	if err != nil {
		return err
	}
	return client.Delete(ctx, kvPath)
}

func (s *remoteSession) IssueCertificate(ctx context.Context, mount, role string, req openbaosession.CertificateRequest) (openbaosession.Certificate, error) {
	if strings.TrimSpace(mount) == "" {
		return openbaosession.Certificate{}, errors.New("vaultaccess: issuing a certificate needs a PKI mount")
	}
	if strings.TrimSpace(role) == "" {
		return openbaosession.Certificate{}, errors.New("vaultaccess: issuing a certificate needs a PKI role")
	}
	if strings.TrimSpace(req.CommonName) == "" {
		return openbaosession.Certificate{}, errors.New("vaultaccess: issuing a certificate needs a common name")
	}
	client, err := s.clientFor(ctx)
	if err != nil {
		return openbaosession.Certificate{}, err
	}
	request := openbao.CertRequest{
		CommonName: req.CommonName,
		AltNames:   append([]string(nil), req.AltNames...),
	}
	if req.TTL > 0 {
		request.TTL = req.TTL.String()
	}
	out, err := client.IssueCert(ctx, mount, role, request)
	if err != nil {
		return openbaosession.Certificate{}, err
	}
	certificate := openbaosession.Certificate{
		CertificatePEM: []byte(out.Certificate),
		PrivateKeyPEM:  []byte(out.PrivateKey),
		IssuingCAPEM:   []byte(out.IssuingCA),
		Serial:         out.Serial,
	}
	return certificate, nil
}

func (s *remoteSession) Authenticated(ctx context.Context) error {
	client, err := s.clientFor(ctx)
	if err != nil {
		return err
	}
	data, err := client.Read(ctx, "auth/token/lookup-self")
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("vaultaccess: the token lookup answered nothing")
	}
	s.observeLease(data)
	return nil
}

func (s *remoteSession) observeLease(data map[string]any) {
	renewable, hasRenewable := data["renewable"].(bool)
	ttl, ok := numberValue(data["ttl"])
	s.mu.Lock()
	defer s.mu.Unlock()
	if hasRenewable {
		s.renewable = renewable
	}
	if !ok || ttl <= 0 {
		return
	}
	s.lease = time.Duration(ttl * float64(time.Second))
	if s.renewable {
		s.renewAt = s.now().Add(s.lease / 2)
	} else {
		s.renewAt = s.now().Add(s.lease)
	}
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		out, err := number.Float64()
		return out, err == nil
	default:
		return 0, false
	}
}

func (s *remoteSession) MountType(ctx context.Context, path string) (string, bool, error) {
	return s.mountType(ctx, "sys/mounts", path)
}

func (s *remoteSession) AuthMountType(ctx context.Context, path string) (string, bool, error) {
	return s.mountType(ctx, "sys/auth", path)
}

func (s *remoteSession) mountType(ctx context.Context, system, path string) (string, bool, error) {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return "", false, nil
	}
	client, err := s.clientFor(ctx)
	if err != nil {
		return "", false, err
	}
	data, err := client.Read(ctx, system)
	if err != nil {
		return "", false, err
	}
	for _, key := range []string{trimmed + "/", trimmed} {
		entry, ok := data[key]
		if !ok {
			continue
		}
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := object["type"].(string)
		return kind, true, nil
	}
	return "", false, nil
}

func (s *remoteSession) Capabilities(ctx context.Context, paths []string) (map[string][]string, error) {
	out := make(map[string][]string, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	client, err := s.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	data, err := client.WriteData(ctx, "sys/capabilities-self", map[string]any{"paths": paths})
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if raw, ok := data[path]; ok {
			out[path] = capabilityList(raw)
		}
	}
	if len(out) == 0 && len(paths) == 1 {
		if raw, ok := data["capabilities"]; ok {
			out[paths[0]] = capabilityList(raw)
		}
	}
	return out, nil
}

func capabilityList(raw any) []string {
	switch value := raw.(type) {
	case []any:
		out := make([]string, 0, len(value))
		for _, entry := range value {
			if text, ok := entry.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return append([]string(nil), value...)
	default:
		return nil
	}
}

func loginAppRole(ctx context.Context, client *http.Client, address, mount, roleID, secretID string) (loginResult, error) {
	return postAuth(ctx, client, address, "auth/"+strings.Trim(mount, "/")+"/login", "", map[string]any{
		"role_id":   roleID,
		"secret_id": secretID,
	})
}

func renewToken(ctx context.Context, client *http.Client, address, token string) (loginResult, error) {
	return postAuth(ctx, client, address, "auth/token/renew-self", token, map[string]any{})
}

func postAuth(ctx context.Context, client *http.Client, address, path, token string, body map[string]any) (loginResult, error) {
	target := strings.TrimRight(address, "/") + "/v1/" + strings.TrimPrefix(path, "/")
	req, err := httpjson.NewRequest(ctx, http.MethodPost, target, body)
	if err != nil {
		return loginResult{}, fmt.Errorf("vaultaccess: building the request for %s: %w", path, err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	payload, err := httpjson.DoLimit[struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}](ctx, client, req, 1<<20)
	if status, ok := errors.AsType[*httpjson.StatusError](err); ok {
		return loginResult{}, fmt.Errorf("vaultaccess: POST %s -> HTTP %d", path, status.Status)
	}
	if err != nil {
		return loginResult{}, fmt.Errorf("vaultaccess: POST %s: %w", path, err)
	}
	if payload.Auth.ClientToken == "" {
		return loginResult{}, fmt.Errorf("vaultaccess: POST %s answered without a client token", path)
	}
	return loginResult{
		token:     payload.Auth.ClientToken,
		lease:     time.Duration(payload.Auth.LeaseDuration) * time.Second,
		renewable: payload.Auth.Renewable,
	}, nil
}

func tokenLease(ctx context.Context, target Target, token string) (time.Duration, bool) {
	client := openbao.New(target.URL, token, openbao.WithHTTPClient(target.HTTPClient))
	data, err := client.Read(ctx, "auth/token/lookup-self")
	if err != nil {
		return 0, false
	}
	ttl, ok := numberValue(data["ttl"])
	if !ok || ttl <= 0 {
		return 0, false
	}
	renewable, _ := data["renewable"].(bool)
	return time.Duration(ttl * float64(time.Second)), renewable
}

func kvDataPath(mount, path string) (string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" {
		return "", errors.New("vaultaccess: a KV read or write needs a mount")
	}
	if path == "" {
		return "", errors.New("vaultaccess: a KV read or write needs a path")
	}
	if strings.HasPrefix(path, mount+"/data/") {
		return path, nil
	}
	return mount + "/data/" + path, nil
}

func kvMetadataPath(mount, path string) (string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" {
		return "", errors.New("vaultaccess: a KV delete needs a mount")
	}
	if path == "" {
		return "", errors.New("vaultaccess: a KV delete needs a path")
	}
	if strings.HasPrefix(path, mount+"/metadata/") {
		return path, nil
	}
	return mount + "/metadata/" + path, nil
}

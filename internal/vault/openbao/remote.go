package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

var (
	ErrRemoteDisabled = errors.New("openbao: remote OpenBao access is disabled")

	ErrInvalidRef = errors.New("openbao: invalid reference")

	ErrInvalidConfig = errors.New("openbao: invalid configuration")

	ErrSuspended = errors.New("openbao: the RemoteOpenBao is suspended")

	ErrUnsupported = errors.New("openbao: unsupported")
)

type Remote struct {
	HTTPClient *http.Client

	TLS *tls.Config

	Enabled bool

	AllowInsecureHTTP bool

	Timeout time.Duration
}

type Target struct {
	URL string

	HTTPClient *http.Client
}

func (r *Remote) Open(ctx context.Context, remote *v1alpha1.RemoteOpenBao) (*Client, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: the remote options are nil", ErrInvalidConfig)
	}
	if remote == nil {
		return nil, fmt.Errorf("%w: the RemoteOpenBao is nil", ErrInvalidRef)
	}
	if !r.Enabled {
		return nil, fmt.Errorf("%w: RemoteOpenBao %s", ErrRemoteDisabled, remote.Name)
	}
	if remote.Spec.Suspend {
		return nil, fmt.Errorf("%w: RemoteOpenBao %s", ErrSuspended, remote.Name)
	}
	target, err := r.Target(remote)
	if err != nil {
		return nil, err
	}
	base := New(target.URL, "", WithHTTPClient(target.HTTPClient))
	auth := remote.Spec.Auth
	switch auth.Method {
	case v1alpha1.RemoteOpenBaoAuthToken:
		if strings.TrimSpace(auth.TokenRef) == "" {
			return nil, fmt.Errorf("%w: RemoteOpenBao %s token auth names no tokenRef", ErrInvalidConfig, remote.Name)
		}
		tokenRef := auth.TokenRef
		src := lease.New(func(ctx context.Context) (string, time.Time, error) {
			token, err := refValue(tokenRef)
			if err != nil {
				return "", time.Time{}, fmt.Errorf("openbao: RemoteOpenBao %s: %w", remote.Name, err)
			}
			return token, leaseUntil(base.WithToken(token).tokenTTL(ctx)), nil
		})
		if _, err := src.Get(ctx); err != nil {
			return nil, err
		}
		return base.with(src, true), nil
	case v1alpha1.RemoteOpenBaoAuthAppRole:
		if auth.AppRole == nil {
			return nil, fmt.Errorf("%w: RemoteOpenBao %s approle auth names no approle block", ErrInvalidConfig, remote.Name)
		}
		roleID, err := refValue(auth.AppRole.RoleIDRef)
		if err != nil {
			return nil, fmt.Errorf("openbao: RemoteOpenBao %s approle: %w", remote.Name, err)
		}
		secretID, err := refValue(auth.AppRole.SecretIDRef)
		if err != nil {
			return nil, fmt.Errorf("openbao: RemoteOpenBao %s approle: %w", remote.Name, err)
		}
		client, err := base.LoginAppRole(ctx, remote.Spec.Mounts.Auth, roleID, secretID)
		if err != nil {
			return nil, fmt.Errorf("openbao: RemoteOpenBao %s: %w", remote.Name, err)
		}
		return client, nil
	case v1alpha1.RemoteOpenBaoAuthCert:
		if auth.Cert == nil || strings.TrimSpace(auth.Cert.CertRef) == "" || strings.TrimSpace(auth.Cert.KeyRef) == "" {
			return nil, fmt.Errorf("%w: RemoteOpenBao %s cert auth names no certRef and keyRef", ErrInvalidConfig, remote.Name)
		}
		return nil, fmt.Errorf("%w: RemoteOpenBao %s uses cert auth, which needs a cert auth role this object does not name", ErrUnsupported, remote.Name)
	case "":
		return nil, fmt.Errorf("%w: RemoteOpenBao %s names no auth method", ErrInvalidConfig, remote.Name)
	default:
		return nil, fmt.Errorf("%w: RemoteOpenBao %s names auth method %q", ErrUnsupported, remote.Name, auth.Method)
	}
}

func (c *Client) tokenTTL(ctx context.Context) time.Duration {
	data, err := c.Read(ctx, "auth/token/lookup-self")
	if err != nil {
		return 0
	}
	ttl, ok := numberValue(data["ttl"])
	if !ok || ttl <= 0 {
		return 0
	}
	return time.Duration(ttl * float64(time.Second))
}

func (r *Remote) Target(remote *v1alpha1.RemoteOpenBao) (Target, error) {
	if remote == nil {
		return Target{}, fmt.Errorf("%w: the RemoteOpenBao is nil", ErrInvalidRef)
	}
	base, err := parseRemoteURL(remote.Spec.URL, r.AllowInsecureHTTP)
	if err != nil {
		return Target{}, fmt.Errorf("%w: RemoteOpenBao %s: %v", ErrInvalidConfig, remote.Name, err)
	}
	tlsConfig, err := r.remoteTLS(remote.Spec.TLS, base)
	if err != nil {
		return Target{}, fmt.Errorf("%w: RemoteOpenBao %s: %v", ErrInvalidConfig, remote.Name, err)
	}
	client, err := r.httpClient(tlsConfig)
	if err != nil {
		return Target{}, fmt.Errorf("%w: RemoteOpenBao %s: %v", ErrInvalidConfig, remote.Name, err)
	}
	return Target{URL: strings.TrimRight(base.String(), "/"), HTTPClient: client}, nil
}

func (r *Remote) remoteTLS(spec v1alpha1.RemoteOpenBaoTLS, base *url.URL) (*tls.Config, error) {
	caRef := strings.TrimSpace(spec.CABundleRef)
	serverName := strings.TrimSpace(spec.ServerName)
	if !spec.Enabled {
		if caRef != "" || serverName != "" {
			return nil, errors.New("tls fields are set but tls.enabled is false")
		}
		return r.baseTLS(), nil
	}
	if base.Scheme != "https" {
		return nil, errors.New("tls is enabled but the url is not https")
	}
	config := r.baseTLS()
	if config == nil {
		config = &tls.Config{}
	}
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	if caRef != "" {
		data, err := readRef(caRef)
		if err != nil {
			return nil, fmt.Errorf("reading the CA bundle: %w", err)
		}
		pool, err := CertPoolFromPEM(data)
		if err != nil {
			return nil, fmt.Errorf("the CA bundle %q does not parse", caRef)
		}
		config.RootCAs = pool
	}
	if serverName != "" {
		config.ServerName = serverName
	}
	return config, nil
}

func (r *Remote) baseTLS() *tls.Config {
	if r.TLS != nil {
		return r.TLS.Clone()
	}
	if r.HTTPClient != nil {
		if transport, ok := r.HTTPClient.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
			return transport.TLSClientConfig.Clone()
		}
	}
	return nil
}

func (r *Remote) httpClient(config *tls.Config) (*http.Client, error) {
	base := *http.DefaultClient
	if r.HTTPClient != nil {
		base = *r.HTTPClient
	}
	if r.Timeout > 0 {
		base.Timeout = r.Timeout
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	cloned, ok := transport.(*http.Transport)
	if !ok {
		if config != nil {
			return nil, errors.New("the HTTP client's transport does not carry TLS settings")
		}
		return &base, nil
	}
	next := cloned.Clone()
	if config != nil {
		next.TLSClientConfig = config
	}
	base.Transport = next
	return &base, nil
}

func parseRemoteURL(raw string, allowHTTP bool) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("the url is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("the url does not parse: %v", err)
	}
	if parsed.Host == "" {
		return nil, errors.New("the url has no host")
	}
	if parsed.User != nil {
		return nil, errors.New("the url must not carry user info")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, errors.New("the url needs https; http is allowed only with the explicit insecure flag")
		}
	default:
		return nil, errors.New("the url needs an http or https scheme")
	}
	parsed.Scheme = scheme
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func readRef(ref string) ([]byte, error) {
	value := strings.TrimSpace(ref)
	if value == "" {
		return nil, errors.New("the reference is empty")
	}
	path := strings.TrimPrefix(value, "file:")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return bytes.TrimSpace(data), nil
}

func refValue(ref string) (string, error) {
	data, err := readRef(ref)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("%s is empty", ref)
	}
	return string(data), nil
}

func (c *Client) Authenticated(ctx context.Context) error {
	data, err := c.Read(ctx, "auth/token/lookup-self")
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("openbao: the token lookup answered nothing")
	}
	return nil
}

func (c *Client) MountType(ctx context.Context, path string) (string, bool, error) {
	return c.mountType(ctx, "sys/mounts", path)
}

func (c *Client) AuthMountType(ctx context.Context, path string) (string, bool, error) {
	return c.mountType(ctx, "sys/auth", path)
}

func (c *Client) mountType(ctx context.Context, system, path string) (string, bool, error) {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return "", false, nil
	}
	mounts, err := c.mounts(ctx, system)
	if err != nil {
		return "", false, err
	}
	if m, ok := mounts[trimmed]; ok {
		return m.Type, true, nil
	}
	return "", false, nil
}

func (c *Client) Capabilities(ctx context.Context, paths []string) (map[string][]string, error) {
	out := make(map[string][]string, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	data, err := c.WriteData(ctx, "sys/capabilities-self", map[string]any{"paths": paths})
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

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
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

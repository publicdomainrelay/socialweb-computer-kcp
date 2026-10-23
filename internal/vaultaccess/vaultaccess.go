package vaultaccess

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials/openbaosession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

var (
	ErrRemoteDisabled = errors.New("vaultaccess: remote OpenBao access is disabled")

	ErrInvalidRef = errors.New("vaultaccess: invalid reference")

	ErrInvalidConfig = errors.New("vaultaccess: invalid configuration")

	ErrSuspended = errors.New("vaultaccess: the RemoteOpenBao is suspended")

	ErrUnsupported = errors.New("vaultaccess: unsupported")

	ErrNoStore = errors.New("vaultaccess: local access needs a kcp store")
)

type Ref interface {
	vaultRef()
}

type LocalRef struct {
	Workspace string

	Name string
}

func (LocalRef) vaultRef() {}

type RemoteRef struct {
	Remote *v1alpha1.RemoteOpenBao
}

func (RemoteRef) vaultRef() {}

type Session interface {
	ReadKV(ctx context.Context, mount, path string, version int) (map[string]any, error)

	WriteKV(ctx context.Context, mount, path string, data map[string]any) error

	DeleteKV(ctx context.Context, mount, path string) error
}

type CertificateIssuer interface {
	IssueCertificate(ctx context.Context, mount, role string, req openbaosession.CertificateRequest) (openbaosession.Certificate, error)
}

var _ Session = (*openbaosession.Session)(nil)

var _ CertificateIssuer = (*openbaosession.Session)(nil)

type Resolver struct {
	Store *kcpstore.Store

	HTTPClient *http.Client

	TLS *tls.Config

	RemoteEnabled bool

	AllowInsecureHTTP bool

	Timeout time.Duration

	Now func() time.Time
}

type Target struct {
	URL string

	HTTPClient *http.Client
}

func (r *Resolver) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) Resolve(ctx context.Context, ref Ref) (Session, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: the resolver is nil", ErrInvalidConfig)
	}
	switch value := ref.(type) {
	case LocalRef:
		return r.ResolveLocal(ctx, value)
	case RemoteRef:
		return r.ResolveRemote(ctx, value)
	default:
		return nil, fmt.Errorf("%w: unknown reference", ErrInvalidRef)
	}
}

func (r *Resolver) ResolveLocal(ctx context.Context, ref LocalRef) (Session, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: the resolver is nil", ErrInvalidConfig)
	}
	if r.Store == nil {
		return nil, ErrNoStore
	}
	source := &openbaosession.KCPStoreSource{Store: r.Store}
	srv, err := source.Server(ctx, ref.Workspace, ref.Name)
	if err != nil {
		return nil, err
	}
	srv.Timeout = cmp.Or(srv.Timeout, r.Timeout)
	return openbaosession.Open(ctx, srv)
}

func (r *Resolver) ResolveRemote(ctx context.Context, ref RemoteRef) (Session, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: the resolver is nil", ErrInvalidConfig)
	}
	remote := ref.Remote
	if remote == nil {
		return nil, fmt.Errorf("%w: the RemoteOpenBao is nil", ErrInvalidRef)
	}
	if !r.RemoteEnabled {
		return nil, fmt.Errorf("%w: RemoteOpenBao %s", ErrRemoteDisabled, remote.Name)
	}
	if remote.Spec.Suspend {
		return nil, fmt.Errorf("%w: RemoteOpenBao %s", ErrSuspended, remote.Name)
	}
	target, err := r.RemoteTarget(remote)
	if err != nil {
		return nil, err
	}
	auth := remote.Spec.Auth
	switch auth.Method {
	case v1alpha1.RemoteOpenBaoAuthToken:
		if strings.TrimSpace(auth.TokenRef) == "" {
			return nil, fmt.Errorf("%w: RemoteOpenBao %s token auth names no tokenRef", ErrInvalidConfig, remote.Name)
		}
		token, err := readRef(auth.TokenRef)
		if err != nil {
			return nil, fmt.Errorf("vaultaccess: RemoteOpenBao %s: %w", remote.Name, err)
		}
		if len(token) == 0 {
			return nil, fmt.Errorf("%w: RemoteOpenBao %s tokenRef %q is empty", ErrInvalidConfig, remote.Name, auth.TokenRef)
		}
		lease, renewable := tokenLease(ctx, target, string(token))
		return newRemoteSession(target, loginResult{token: string(token), lease: lease, renewable: renewable}, nil, r.now), nil
	case v1alpha1.RemoteOpenBaoAuthAppRole:
		if auth.AppRole == nil {
			return nil, fmt.Errorf("%w: RemoteOpenBao %s approle auth names no approle block", ErrInvalidConfig, remote.Name)
		}
		roleID, err := refValue(auth.AppRole.RoleIDRef)
		if err != nil {
			return nil, fmt.Errorf("vaultaccess: RemoteOpenBao %s approle: %w", remote.Name, err)
		}
		secretID, err := refValue(auth.AppRole.SecretIDRef)
		if err != nil {
			return nil, fmt.Errorf("vaultaccess: RemoteOpenBao %s approle: %w", remote.Name, err)
		}
		mount := strings.Trim(strings.TrimSpace(remote.Spec.Mounts.Auth), "/")
		mount = cmp.Or(mount, "approle")
		login := func(ctx context.Context) (loginResult, error) {
			return loginAppRole(ctx, target.HTTPClient, target.URL, mount, roleID, secretID)
		}
		result, err := login(ctx)
		if err != nil {
			return nil, fmt.Errorf("vaultaccess: RemoteOpenBao %s: %w", remote.Name, err)
		}
		return newRemoteSession(target, result, login, r.now), nil
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

func (r *Resolver) RemoteTarget(remote *v1alpha1.RemoteOpenBao) (Target, error) {
	if r == nil {
		return Target{}, fmt.Errorf("%w: the resolver is nil", ErrInvalidConfig)
	}
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

func (r *Resolver) remoteTLS(spec v1alpha1.RemoteOpenBaoTLS, base *url.URL) (*tls.Config, error) {
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
	} else {
		config = config.Clone()
	}
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	if caRef != "" {
		pem, err := readRef(caRef)
		if err != nil {
			return nil, fmt.Errorf("reading the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the CA bundle %q does not parse", caRef)
		}
		config.RootCAs = pool
	}
	if serverName != "" {
		config.ServerName = serverName
	}
	return config, nil
}

func (r *Resolver) baseTLS() *tls.Config {
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

func (r *Resolver) httpClient(config *tls.Config) (*http.Client, error) {
	var base http.Client
	if r.HTTPClient != nil {
		base = *r.HTTPClient
	} else {
		base = *http.DefaultClient
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
		base.Transport = transport
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

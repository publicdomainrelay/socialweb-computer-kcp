package component

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/rest"
)

var ErrRemoteVaultDisabled = errors.New("component: RemoteOpenBao vault access is disabled")

type KCPClient interface {
	Workspace() string
	RESTConfig() *rest.Config
}

type KCPClients interface {
	Client(workspace string) (KCPClient, error)
}

type KCPClientsFunc func(workspace string) (KCPClient, error)

func (f KCPClientsFunc) Client(workspace string) (KCPClient, error) {
	return f(workspace)
}

type CredentialRef struct {
	Server  string
	Mount   string
	Path    string
	Version int
	Keys    []string
}

func (r CredentialRef) Check() error {
	switch {
	case r.Mount == "":
		return errors.New("component: the credential ref has no mount")
	case r.Path == "":
		return fmt.Errorf("component: the credential ref in mount %q has no path", r.Mount)
	case r.Version < 0:
		return fmt.Errorf("component: the credential ref %s/%s has a negative version", r.Mount, r.Path)
	}
	return nil
}

type Credential struct {
	Ref       CredentialRef
	Data      map[string]any
	LeaseID   string
	Renewable bool
	ExpiresAt time.Time
}

func (c Credential) Value(key string) (string, bool) {
	v, ok := c.Data[key]
	if !ok {
		return "", false
	}
	switch value := v.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	default:
		return "", false
	}
}

func (c Credential) Bytes(key string) ([]byte, bool) {
	v, ok := c.Data[key]
	if !ok {
		return nil, false
	}
	switch value := v.(type) {
	case string:
		return []byte(value), true
	case []byte:
		return append([]byte(nil), value...), true
	default:
		return nil, false
	}
}

type CredentialResolver interface {
	Resolve(ctx context.Context, ref CredentialRef) (Credential, error)
}

type CredentialResolverFunc func(ctx context.Context, ref CredentialRef) (Credential, error)

func (f CredentialResolverFunc) Resolve(ctx context.Context, ref CredentialRef) (Credential, error) {
	return f(ctx, ref)
}

type CredentialWriter interface {
	WriteCredential(ctx context.Context, ref CredentialRef, data map[string]any) error
}

type CredentialDeleter interface {
	DeleteCredential(ctx context.Context, ref CredentialRef) error
}

type CertificateRequest struct {
	Mount      string
	Role       string
	CommonName string
	AltNames   []string
	IPSANs     []string
	TTL        time.Duration
}

type Certificate struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	IssuingCAPEM   []byte
	CAChainPEM     [][]byte
	Serial         string
}

type VaultClient interface {
	ReadKV(ctx context.Context, mount, path string) (map[string]any, error)
	IssueCertificate(ctx context.Context, req CertificateRequest) (Certificate, error)
}

type VaultClients interface {
	Client(ctx context.Context, server string) (VaultClient, error)
}

type VaultClientsFunc func(ctx context.Context, server string) (VaultClient, error)

func (f VaultClientsFunc) Client(ctx context.Context, server string) (VaultClient, error) {
	return f(ctx, server)
}

func (c Certificate) TLSConfig() (*tls.Config, error) {
	if len(c.CertificatePEM) == 0 || len(c.PrivateKeyPEM) == 0 {
		return nil, errors.New("component: a TLS config needs a certificate and its key")
	}
	pair, err := tls.X509KeyPair(c.CertificatePEM, c.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("component: the certificate and key: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	chain := c.CAChainPEM
	if len(chain) == 0 && len(c.IssuingCAPEM) > 0 {
		chain = [][]byte{c.IssuingCAPEM}
	}
	if len(chain) > 0 {
		pool := x509.NewCertPool()
		for _, entry := range chain {
			if !pool.AppendCertsFromPEM(entry) {
				return nil, errors.New("component: the certificate CA chain holds no certificate")
			}
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

type VaultSession interface {
	VaultClient

	WriteKV(ctx context.Context, mount, path string, data map[string]any) error

	DeleteKV(ctx context.Context, mount, path string) error
}

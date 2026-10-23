package pds

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsreload"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "Pds"

const DefaultServeHost = "127.0.0.1"

const (
	RepoKeyDir        = "repo-keys"
	SessionSecretFile = "session-secret"
	RotationKeyFile   = "plc-rotation.key"
)

type Config struct {
	Deps     component.Deps
	Key      component.Key
	Mode     atpds.Mode
	StateDir string
	BlobsDir string
	CertsDir string

	Identity  v1alpha1.ComponentIdentity
	VaultRef  *commonv1.ObjectRef
	Hostnames []string

	PublicBaseURL string
	ServiceDID    string
	AlsoKnownAs   []string

	RequireIngress bool
	IngressRef     *commonv1.ObjectRef
	PublicURL      string
	StableHost     string

	ServeHost  string
	ServePort  int
	HealthPath string
	TLS        *v1alpha1.ComponentTLS
	TLSConfig  *tls.Config
	CertFile   string
	KeyFile    string
	CABundle   string

	Directory identity.Directory
	Resolver  atpds.DIDResolver
	PLC       atpds.PLCSubmitter
	PLCURL    string
	PLCClient *http.Client
	Blobs     atpds.BlobStore

	Credentials       component.CredentialResolver
	SessionSecret     []byte
	SessionSecretPath string

	RepoKeyStore atpds.RepoKeyStore
	RepoKeyPath  string

	RotationKey     atcrypto.PrivateKey
	RotationKeyPath string

	OAuth *OAuthOptions

	AvailableUserDomains []string
	ContactEmail         string
	Version              string
	ServiceAuthAudiences []string
	MaxBlobSize          int64
	MaxRequestBytes      int64
	AllowExplicitDID     bool
}

type OAuthOptions struct {
	Issuer                  string
	Signer                  atcrypto.PrivateKey
	SignerRef               *component.CredentialRef
	SignerField             string
	HTTPClient              *http.Client
	AccessTokenTTL          time.Duration
	RefreshTokenTTL         time.Duration
	AuthorizationCodeTTL    time.Duration
	PARTTL                  time.Duration
	AllowMissingDPoPNonce   bool
	DisableLocalhostClients bool
	Scopes                  []string
}

func Factory(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
	cfg, err := ConfigFrom(deps, spec)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

func Register(runtime *component.Runtime) error {
	return runtime.Register(Kind, Factory)
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.PdsSpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("pds: decoding the component spec: %w", err)
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	cfg := Config{
		Deps:        deps,
		Key:         component.Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name},
		StateDir:    stateDir,
		ServeHost:   DefaultServeHost,
		Credentials: deps.Credentials,
		Hostnames:   append([]string(nil), decoded.Hostnames...),
		VaultRef:    decoded.VaultRef,
	}
	if decoded.Storage != nil {
		if decoded.Storage.DataDir != "" {
			cfg.StateDir = decoded.Storage.DataDir
		}
		cfg.BlobsDir = strings.TrimSpace(decoded.Storage.BlobsDir)
		cfg.CertsDir = strings.TrimSpace(decoded.Storage.CertsDir)
	}
	if decoded.Identity != nil {
		cfg.Identity = *decoded.Identity
		cfg.ServiceDID = strings.TrimSpace(decoded.Identity.Did)
		if handle := strings.TrimSpace(decoded.Identity.Handle); handle != "" {
			cfg.AlsoKnownAs = []string{"at://" + handle}
		}
	}
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(decoded.PublicBaseURL), "/")
	cfg.RequireIngress = decoded.RequireIngress
	cfg.IngressRef = decoded.IngressRef
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(decoded.PublicURL), "/")
	cfg.StableHost = strings.TrimSpace(decoded.StableHost)
	cfg.PLCURL = strings.TrimRight(strings.TrimSpace(decoded.PlcURL), "/")
	if cfg.PLCURL == "" && decoded.Network != nil {
		cfg.PLCURL = strings.TrimRight(strings.TrimSpace(decoded.Network.PLCDirectoryURL), "/")
	}
	if stateDir != "" {
		cfg.RepoKeyPath = filepath.Join(stateDir, RepoKeyDir)
		cfg.SessionSecretPath = filepath.Join(stateDir, SessionSecretFile)
		cfg.RotationKeyPath = filepath.Join(stateDir, RotationKeyFile)
	}
	cfg.AllowExplicitDID = decoded.AllowExplicitDID
	switch strings.ToLower(strings.TrimSpace(string(decoded.Mode))) {
	case "", "full":
		cfg.Mode = atpds.ModeFull
	case "repo", "read-only", "readonly":
		cfg.Mode = atpds.ModeRepo
	default:
		return Config{}, fmt.Errorf("pds: unknown mode %q", decoded.Mode)
	}
	if decoded.Serve != nil {
		if host := strings.TrimSpace(decoded.Serve.Host); host != "" {
			cfg.ServeHost = host
		}
		cfg.ServePort = int(decoded.Serve.Port)
		cfg.HealthPath = strings.TrimSpace(decoded.Serve.HealthPath)
	}
	if decoded.TLS != nil {
		tlsSpec := *decoded.TLS
		cfg.TLS = &tlsSpec
		if tlsSpec.Enabled {
			cfg.CertFile = filePath(tlsSpec.CertRef)
			cfg.KeyFile = keyFileFor(cfg.CertFile)
			cfg.CABundle = strings.TrimSpace(tlsSpec.CABundle)
		}
	}
	if cfg.CABundle == "" && decoded.Network != nil {
		cfg.CABundle = strings.TrimSpace(decoded.Network.CABundle)
	}
	if decoded.ServiceAuth != nil {
		cfg.ServiceAuthAudiences = append([]string(nil), decoded.ServiceAuth.Audience...)
	}
	if decoded.OAuth != nil {
		cfg.OAuth = &OAuthOptions{
			Issuer:                  strings.TrimRight(strings.TrimSpace(decoded.OAuth.Issuer), "/"),
			AccessTokenTTL:          time.Duration(decoded.OAuth.AccessTokenTTLSeconds) * time.Second,
			RefreshTokenTTL:         time.Duration(decoded.OAuth.RefreshTokenTTLSeconds) * time.Second,
			DisableLocalhostClients: decoded.OAuth.DisableLocalhostClients || !decoded.OAuth.AllowLocalhostClients,
			Scopes:                  append([]string(nil), decoded.OAuth.Scopes...),
		}
	}
	if decoded.MaxBlobSize != nil {
		cfg.MaxBlobSize = *decoded.MaxBlobSize
	}
	if decoded.MaxRequestBytes != nil {
		cfg.MaxRequestBytes = *decoded.MaxRequestBytes
	}
	if decoded.ContactEmail != "" {
		cfg.ContactEmail = strings.TrimSpace(decoded.ContactEmail)
	}
	cfg.StateDir = strings.TrimSpace(cfg.StateDir)
	return cfg, nil
}

func filePath(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"file://", "file:"} {
		if strings.HasPrefix(raw, prefix) {
			return strings.TrimPrefix(raw, prefix)
		}
	}
	return raw
}

func keyFileFor(certFile string) string {
	certFile = strings.TrimSpace(certFile)
	if certFile == "" {
		return ""
	}
	ext := filepath.Ext(certFile)
	switch strings.ToLower(ext) {
	case ".crt", ".cert", ".pem":
		return strings.TrimSuffix(certFile, ext) + ".key"
	}
	return certFile + ".key"
}

func buildTLSConfig(cfg Config) (*tls.Config, error) {
	if cfg.TLSConfig != nil {
		return cfg.TLSConfig.Clone(), nil
	}
	if cfg.TLS == nil || !cfg.TLS.Enabled {
		return nil, nil
	}
	var tlsConfig *tls.Config
	if cfg.CertFile == "" {
		if cfg.Deps.TLS == nil {
			return nil, errors.New("pds: tls is enabled but no certificate is configured")
		}
		tlsConfig = cfg.Deps.TLS.Clone()
	} else {
		reloader, err := tlsreload.New(cfg.CertFile, []string{cfg.KeyFile, cfg.CertFile}, cfg.Deps.Log)
		if err != nil {
			if cfg.Deps.TLS == nil {
				return nil, fmt.Errorf("pds: loading the tls certificate: %w", err)
			}
			tlsConfig = cfg.Deps.TLS.Clone()
		} else {
			tlsConfig = reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
		}
	}
	if cfg.CABundle != "" {
		raw := []byte(cfg.CABundle)
		pool := tlsConfig.RootCAs
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(raw) {
			return nil, errors.New("pds: the tls ca bundle holds no certificate")
		}
		tlsConfig.RootCAs = pool
		if tlsConfig.ClientCAs == nil {
			tlsConfig.ClientCAs = pool
		}
	}
	return tlsConfig, nil
}

package component

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials/openbaosession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
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

type cachedVaultSession struct {
	client VaultSession

	mount string

	until time.Time
}

type OpenBaoVaultClients struct {
	Source openbaosession.Source

	Workspace string

	LoginFor time.Duration

	Open func(ctx context.Context, srv openbaosession.Server) (VaultSession, error)

	mu sync.Mutex

	sessions map[string]cachedVaultSession
}

var _ VaultClients = (*OpenBaoVaultClients)(nil)

func NewOpenBaoVaultClients(store *kcpstore.Store, workspace, client string) (*OpenBaoVaultClients, error) {
	if store == nil {
		return nil, errors.New("component: the OpenBao vault clients need a kcp store")
	}
	return NewOpenBaoVaultClientsWithSource(&openbaosession.KCPStoreSource{Store: store, Client: client}, workspace)
}

func NewVaultClients(store *kcpstore.Store, workspace, client string) (*OpenBaoVaultClients, error) {
	return NewOpenBaoVaultClients(store, workspace, client)
}

func NewOpenBaoVaultClientsWithSource(source openbaosession.Source, workspace string) (*OpenBaoVaultClients, error) {
	if source == nil {
		return nil, errors.New("component: the OpenBao vault clients need an OpenBaoServer source")
	}
	if workspace == "" {
		workspace = "root"
	}
	out := &OpenBaoVaultClients{
		Source:    source,
		Workspace: workspace,
		LoginFor:  openbaosession.DefaultLoginFor,
		sessions:  map[string]cachedVaultSession{},
	}
	out.Open = func(ctx context.Context, srv openbaosession.Server) (VaultSession, error) {
		session, err := openbaosession.Open(ctx, srv)
		if err != nil {
			return nil, err
		}
		return openBaoVaultSession{session: session}, nil
	}
	return out, nil
}

func (c *OpenBaoVaultClients) Client(ctx context.Context, server string) (VaultClient, error) {
	if strings.HasPrefix(strings.TrimSpace(server), "RemoteOpenBao/") {
		return nil, fmt.Errorf("%w: %s", ErrRemoteVaultDisabled, server)
	}
	if c == nil || c.Source == nil {
		return nil, errors.New("component: the OpenBao vault clients have no source")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workspace, name := openbaosession.ParseServer(server, c.Workspace)
	if name == "" {
		return nil, openbaosession.ErrNoServer
	}
	key := workspace + "|" + name
	loginFor := c.LoginFor
	if loginFor <= 0 {
		loginFor = openbaosession.DefaultLoginFor
	}
	c.mu.Lock()
	cached, ok := c.sessions[key]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.until) {
		return &openBaoVaultClient{session: cached.client, mount: cached.mount}, nil
	}
	srv, err := c.Source.Server(ctx, workspace, name)
	if err != nil {
		return nil, err
	}
	open := c.Open
	if open == nil {
		open = func(ctx context.Context, srv openbaosession.Server) (VaultSession, error) {
			session, err := openbaosession.Open(ctx, srv)
			if err != nil {
				return nil, err
			}
			return openBaoVaultSession{session: session}, nil
		}
	}
	session, err := open(ctx, srv)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = map[string]cachedVaultSession{}
	}
	c.sessions[key] = cachedVaultSession{client: session, mount: srv.KVMount, until: time.Now().Add(loginFor)}
	c.mu.Unlock()
	return &openBaoVaultClient{session: session, mount: srv.KVMount}, nil
}

type openBaoVaultClient struct {
	session VaultSession

	mount string
}

func (c *openBaoVaultClient) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	return c.session.ReadKV(ctx, c.mountOr(mount), path)
}

func (c *openBaoVaultClient) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	return c.session.WriteKV(ctx, c.mountOr(mount), path, data)
}

func (c *openBaoVaultClient) DeleteKV(ctx context.Context, mount, path string) error {
	return c.session.DeleteKV(ctx, c.mountOr(mount), path)
}

func (c *openBaoVaultClient) IssueCertificate(ctx context.Context, req CertificateRequest) (Certificate, error) {
	return c.session.IssueCertificate(ctx, req)
}

func (c *openBaoVaultClient) mountOr(mount string) string {
	if mount != "" {
		return mount
	}
	return c.mount
}

type openBaoVaultSession struct {
	session *openbaosession.Session
}

func (s openBaoVaultSession) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	return s.session.ReadKV(ctx, mount, path, 0)
}

func (s openBaoVaultSession) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	return s.session.WriteKV(ctx, mount, path, data)
}

func (s openBaoVaultSession) DeleteKV(ctx context.Context, mount, path string) error {
	return s.session.DeleteKV(ctx, mount, path)
}

func (s openBaoVaultSession) IssueCertificate(ctx context.Context, req CertificateRequest) (Certificate, error) {
	out, err := s.session.IssueCertificate(ctx, req.Mount, req.Role, openbaosession.CertificateRequest{
		CommonName: req.CommonName,
		AltNames:   append([]string(nil), req.AltNames...),
		IPSANs:     append([]string(nil), req.IPSANs...),
		TTL:        req.TTL,
	})
	if err != nil {
		return Certificate{}, err
	}
	certificate := Certificate{
		CertificatePEM: append([]byte(nil), out.CertificatePEM...),
		PrivateKeyPEM:  append([]byte(nil), out.PrivateKeyPEM...),
		IssuingCAPEM:   append([]byte(nil), out.IssuingCAPEM...),
		Serial:         out.Serial,
	}
	for _, entry := range out.CAChainPEM {
		certificate.CAChainPEM = append(certificate.CAChainPEM, append([]byte(nil), entry...))
	}
	return certificate, nil
}

type KCPClientSet struct {
	mu sync.RWMutex

	base *rest.Config

	configs map[string]*rest.Config
}

var _ KCPClients = (*KCPClientSet)(nil)

func NewKCPClients(cfg *rest.Config) (*KCPClientSet, error) {
	base, err := kcpConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &KCPClientSet{base: base, configs: map[string]*rest.Config{}}, nil
}

func NewKCPClientsFromStore(store *kcpstore.Store) (*KCPClientSet, error) {
	if store == nil {
		return nil, errors.New("component: the kcp clients need a kcp store")
	}
	return NewKCPClients(store.Config())
}

func NewKCPClientsFromKubeconfig(kubeconfig string) (*KCPClientSet, error) {
	if kubeconfig == "" {
		return nil, errors.New("component: the kcp clients need a kubeconfig")
	}
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("component: the kcp front rest config: %w", err)
	}
	return NewKCPClients(cfg)
}

func (s *KCPClientSet) Set(workspace string, cfg *rest.Config) error {
	if s == nil {
		return errors.New("component: the kcp client set is nil")
	}
	normalized, err := kcpConfig(cfg)
	if err != nil {
		return err
	}
	workspace = kcpWorkspace(workspace)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs == nil {
		s.configs = map[string]*rest.Config{}
	}
	s.configs[workspace] = normalized
	return nil
}

func (s *KCPClientSet) Client(workspace string) (KCPClient, error) {
	if s == nil {
		return nil, errors.New("component: the kcp client set is nil")
	}
	workspace = kcpWorkspace(workspace)
	s.mu.RLock()
	cfg := s.configs[workspace]
	base := s.base
	s.mu.RUnlock()
	if cfg == nil {
		cfg = base
	}
	if cfg == nil {
		return nil, errors.New("component: the kcp client set has no rest config")
	}
	return &kcpClient{workspace: workspace, cfg: rest.CopyConfig(cfg)}, nil
}

type kcpClient struct {
	workspace string

	cfg *rest.Config
}

func (c *kcpClient) Workspace() string { return c.workspace }

func (c *kcpClient) RESTConfig() *rest.Config { return rest.CopyConfig(c.cfg) }

func kcpConfig(cfg *rest.Config) (*rest.Config, error) {
	if cfg == nil {
		return nil, errors.New("component: the kcp clients need a rest config")
	}
	out := rest.CopyConfig(cfg)
	out.Host = ref.BaseHost(out.Host)
	if out.Host == "" {
		return nil, errors.New("component: the kcp rest config has no host")
	}
	return out, nil
}

func kcpWorkspace(workspace string) string {
	if workspace == "" {
		return "root"
	}
	return workspace
}

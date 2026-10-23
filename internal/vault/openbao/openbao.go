package openbao

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials/openbaosession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

type cachedVaultSession struct {
	client component.VaultSession

	mount string
}

type vaultServerKey struct {
	workspace string
	name      string
}

type Clients struct {
	Source openbaosession.Source

	Workspace string

	LoginFor time.Duration

	Open func(ctx context.Context, srv openbaosession.Server) (component.VaultSession, error)

	mu sync.Mutex

	sessions *lease.Keyed[vaultServerKey, cachedVaultSession]
}

var _ component.VaultClients = (*Clients)(nil)

func New(store *kcpstore.Store, workspace, client string) (*Clients, error) {
	if store == nil {
		return nil, errors.New("openbao: the OpenBao vault clients need a kcp store")
	}
	return NewWithSource(&openbaosession.KCPStoreSource{Store: store, Client: client}, workspace)
}

func NewWithSource(source openbaosession.Source, workspace string) (*Clients, error) {
	if source == nil {
		return nil, errors.New("openbao: the OpenBao vault clients need an OpenBaoServer source")
	}
	if workspace == "" {
		workspace = "root"
	}
	out := &Clients{
		Source:    source,
		Workspace: workspace,
		LoginFor:  openbaosession.DefaultLoginFor,
	}
	out.Open = func(ctx context.Context, srv openbaosession.Server) (component.VaultSession, error) {
		session, err := openbaosession.Open(ctx, srv)
		if err != nil {
			return nil, err
		}
		return openBaoVaultSession{session: session}, nil
	}
	return out, nil
}

func (c *Clients) Client(ctx context.Context, server string) (component.VaultClient, error) {
	if strings.HasPrefix(strings.TrimSpace(server), "RemoteOpenBao/") {
		return nil, fmt.Errorf("%w: %s", component.ErrRemoteVaultDisabled, server)
	}
	if c == nil || c.Source == nil {
		return nil, errors.New("openbao: the OpenBao vault clients have no source")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workspace, name := openbaosession.ParseServer(server, c.Workspace)
	if name == "" {
		return nil, openbaosession.ErrNoServer
	}
	cached, err := c.leases().Get(ctx, vaultServerKey{workspace: workspace, name: name})
	if err != nil {
		return nil, err
	}
	return &openBaoVaultClient{session: cached.client, mount: cached.mount}, nil
}

func (c *Clients) leases() *lease.Keyed[vaultServerKey, cachedVaultSession] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions == nil {
		c.sessions = lease.NewKeyed(c.login)
	}
	return c.sessions
}

func (c *Clients) login(ctx context.Context, key vaultServerKey) (cachedVaultSession, time.Time, error) {
	loginFor := c.LoginFor
	if loginFor <= 0 {
		loginFor = openbaosession.DefaultLoginFor
	}
	srv, err := c.Source.Server(ctx, key.workspace, key.name)
	if err != nil {
		return cachedVaultSession{}, time.Time{}, err
	}
	open := c.Open
	if open == nil {
		open = func(ctx context.Context, srv openbaosession.Server) (component.VaultSession, error) {
			session, err := openbaosession.Open(ctx, srv)
			if err != nil {
				return nil, err
			}
			return openBaoVaultSession{session: session}, nil
		}
	}
	session, err := open(ctx, srv)
	if err != nil {
		return cachedVaultSession{}, time.Time{}, err
	}
	until := time.Now().Add(loginFor)
	if at := vaultSessionRefreshAt(session); !at.IsZero() && at.Before(until) {
		until = at
	}
	return cachedVaultSession{client: session, mount: srv.KVMount}, until, nil
}

func vaultSessionRefreshAt(session component.VaultSession) time.Time {
	if refresh, ok := session.(interface{ RefreshAt() time.Time }); ok {
		return refresh.RefreshAt()
	}
	return time.Time{}
}

type openBaoVaultClient struct {
	session component.VaultSession

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

func (c *openBaoVaultClient) IssueCertificate(ctx context.Context, req component.CertificateRequest) (component.Certificate, error) {
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

func (s openBaoVaultSession) RefreshAt() time.Time {
	return s.session.RefreshAt()
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

func (s openBaoVaultSession) IssueCertificate(ctx context.Context, req component.CertificateRequest) (component.Certificate, error) {
	out, err := s.session.IssueCertificate(ctx, req.Mount, req.Role, openbaosession.CertificateRequest{
		CommonName: req.CommonName,
		AltNames:   append([]string(nil), req.AltNames...),
		IPSANs:     append([]string(nil), req.IPSANs...),
		TTL:        req.TTL,
	})
	if err != nil {
		return component.Certificate{}, err
	}
	certificate := component.Certificate{
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

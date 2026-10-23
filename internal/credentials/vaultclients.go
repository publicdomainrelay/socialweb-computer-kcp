package credentials

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

type KCPStoreSource struct {
	Store *kcpstore.Store

	Client string
}

func (s *KCPStoreSource) Server(ctx context.Context, workspace, name string) (openbao.Server, error) {
	if s == nil || s.Store == nil {
		return openbao.Server{}, errors.New("credentials: the OpenBaoServer source needs a kcp store")
	}
	if name == "" {
		return openbao.Server{}, openbao.ErrNoServer
	}
	if workspace == "" {
		workspace = "root"
	}
	obj, err := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](s.Store, v1alpha1.OpenBaoServerGVR).Get(ctx, ref.New(workspace, "", name))
	if err != nil {
		return openbao.Server{}, fmt.Errorf("credentials: reading OpenBaoServer %s/%s: %w", workspace, name, err)
	}
	if obj == nil {
		return openbao.Server{}, fmt.Errorf("credentials: OpenBaoServer %s/%s is not there", workspace, name)
	}
	return openbao.ServerOf(workspace, name, obj.Status, s.Client)
}

type Clients struct {
	Logins *openbao.Logins

	Workspace string
}

var _ component.VaultClients = (*Clients)(nil)

func NewClients(store *kcpstore.Store, workspace, client string) (*Clients, error) {
	if store == nil {
		return nil, errors.New("credentials: the OpenBao vault clients need a kcp store")
	}
	return NewClientsWith(openbao.NewLogins(&KCPStoreSource{Store: store, Client: client}), workspace), nil
}

func NewClientsWith(logins *openbao.Logins, workspace string) *Clients {
	if workspace == "" {
		workspace = "root"
	}
	return &Clients{Logins: logins, Workspace: workspace}
}

func (c *Clients) Client(ctx context.Context, server string) (component.VaultClient, error) {
	if strings.HasPrefix(strings.TrimSpace(server), "RemoteOpenBao/") {
		return nil, fmt.Errorf("%w: %s", component.ErrRemoteVaultDisabled, server)
	}
	if c == nil || c.Logins == nil {
		return nil, errors.New("credentials: the OpenBao vault clients have no logins")
	}
	workspace, name := openbao.ParseServer(server, c.Workspace)
	if name == "" {
		return nil, openbao.ErrNoServer
	}
	client, srv, err := c.Logins.Client(ctx, workspace, name)
	if err != nil {
		return nil, err
	}
	return &componentClient{client: client, mount: srv.KVMount}, nil
}

type componentClient struct {
	client *openbao.Client

	mount string
}

func (c *componentClient) mountOr(mount string) string {
	if mount = strings.Trim(mount, "/"); mount != "" {
		return mount
	}
	return strings.Trim(c.mount, "/")
}

func kvDataPath(mount, path string) string {
	path = strings.Trim(path, "/")
	if strings.HasPrefix(path, mount+"/data/") {
		return path
	}
	return mount + "/data/" + path
}

func (c *componentClient) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	return c.client.GetKV(ctx, kvDataPath(c.mountOr(mount), path))
}

func (c *componentClient) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	return c.client.PutKV(ctx, kvDataPath(c.mountOr(mount), path), data)
}

func (c *componentClient) DeleteKV(ctx context.Context, mount, path string) error {
	mount = c.mountOr(mount)
	path = strings.TrimPrefix(strings.Trim(path, "/"), mount+"/metadata/")
	return c.client.Delete(ctx, mount+"/metadata/"+path)
}

func (c *componentClient) IssueCertificate(ctx context.Context, req component.CertificateRequest) (component.Certificate, error) {
	if strings.TrimSpace(req.Mount) == "" || strings.TrimSpace(req.Role) == "" || strings.TrimSpace(req.CommonName) == "" {
		return component.Certificate{}, errors.New("credentials: issuing a certificate needs a PKI mount, a role and a common name")
	}
	in := openbao.CertRequest{CommonName: req.CommonName, AltNames: append([]string(nil), req.AltNames...), IPSANs: append([]string(nil), req.IPSANs...)}
	if req.TTL > 0 {
		in.TTL = req.TTL.String()
	}
	out, err := c.client.IssueCert(ctx, req.Mount, req.Role, in)
	if err != nil {
		return component.Certificate{}, err
	}
	certificate := component.Certificate{
		CertificatePEM: []byte(out.Certificate),
		PrivateKeyPEM:  []byte(out.PrivateKey),
		IssuingCAPEM:   []byte(out.IssuingCA),
		Serial:         out.Serial,
	}
	for _, entry := range out.CAChain {
		certificate.CAChainPEM = append(certificate.CAChainPEM, []byte(entry))
	}
	return certificate, nil
}

package openbaosession

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/openbaoclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

const DefaultLoginFor = 10 * time.Minute

var OpenBaoServerGVR = v1alpha1.OpenBaoServerGVR

var (
	ErrNotFound = openbao.ErrNotFound

	ErrForbidden = openbao.ErrForbidden

	ErrNoServer = errors.New("openbaosession: no OpenBaoServer was named")

	ErrNoMaterial = errors.New("openbaosession: the OpenBaoServer reports no client certificate")
)

type Server struct {
	Workspace string

	Name string

	Address string

	CACert []byte

	ClientCert []byte

	ClientKey []byte

	CertMount string

	CertRole string

	KVMount string

	Token string

	Timeout time.Duration
}

type Source interface {
	Server(ctx context.Context, workspace, name string) (Server, error)
}

type SourceFunc func(ctx context.Context, workspace, name string) (Server, error)

func (f SourceFunc) Server(ctx context.Context, workspace, name string) (Server, error) {
	return f(ctx, workspace, name)
}

type KCPStoreSource struct {
	Store *kcpstore.Store

	Client string

	Token string

	CACert []byte

	ClientCert []byte

	ClientKey []byte

	CertMount string

	CertRole string

	KVMount string
}

func (s *KCPStoreSource) Server(ctx context.Context, workspace, name string) (Server, error) {
	if s == nil || s.Store == nil {
		return Server{}, errors.New("openbaosession: the OpenBaoServer source needs a kcp store")
	}
	if name == "" {
		return Server{}, ErrNoServer
	}
	if workspace == "" {
		workspace = "root"
	}
	servers := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](s.Store, OpenBaoServerGVR)
	obj, err := servers.Get(ctx, ref.New(workspace, "", name))
	if err != nil {
		return Server{}, fmt.Errorf("openbaosession: reading OpenBaoServer %s/%s: %w", workspace, name, err)
	}
	if obj == nil {
		return Server{}, fmt.Errorf("openbaosession: OpenBaoServer %s/%s is not there", workspace, name)
	}
	st := obj.Status
	if strings.TrimSpace(st.Address) == "" || st.Refs == nil {
		return Server{}, fmt.Errorf("openbaosession: OpenBaoServer %s/%s reports no address yet (phase %s)", workspace, name, st.Phase)
	}
	out := Server{
		Workspace: workspace,
		Name:      name,
		Address:   strings.TrimRight(strings.TrimSpace(st.Address), "/"),
		CertMount: st.Refs.CertAuthMount,
		CertRole:  st.Refs.CertAuthRole,
		KVMount:   st.Refs.KVMount,
		Token:     s.Token,
	}
	var errs []error
	if out.CACert, err = fileBytes(st.Refs.CA); err != nil {
		errs = append(errs, fmt.Errorf("the OpenBao CA: %w", err))
	}
	client, ok := pickClient(st.Clients, s.Client)
	switch {
	case ok:
		if out.ClientCert, err = fileBytes(client.ClientCert); err != nil {
			errs = append(errs, fmt.Errorf("client %s certificate: %w", client.Name, err))
		}
		if out.ClientKey, err = fileBytes(client.ClientKey); err != nil {
			errs = append(errs, fmt.Errorf("client %s key: %w", client.Name, err))
		}
		if client.CertAuthMount != "" {
			out.CertMount = client.CertAuthMount
		}
		if client.CertAuthRole != "" {
			out.CertRole = client.CertAuthRole
		}
		if client.KVMount != "" {
			out.KVMount = client.KVMount
		}
	case s.Client != "":
		errs = append(errs, fmt.Errorf("the OpenBaoServer reports no client %q", s.Client))
	default:
		if out.ClientCert, err = fileBytes(st.Refs.ClientCert); err != nil {
			errs = append(errs, fmt.Errorf("the OpenBao client certificate: %w", err))
		}
		if out.ClientKey, err = fileBytes(st.Refs.ClientKey); err != nil {
			errs = append(errs, fmt.Errorf("the OpenBao client key: %w", err))
		}
	}
	if len(s.CACert) > 0 {
		out.CACert = append([]byte(nil), s.CACert...)
	}
	if len(s.ClientCert) > 0 {
		out.ClientCert = append([]byte(nil), s.ClientCert...)
	}
	if len(s.ClientKey) > 0 {
		out.ClientKey = append([]byte(nil), s.ClientKey...)
	}
	if s.CertMount != "" {
		out.CertMount = s.CertMount
	}
	if s.CertRole != "" {
		out.CertRole = s.CertRole
	}
	if s.KVMount != "" {
		out.KVMount = s.KVMount
	}
	if len(errs) > 0 {
		return Server{}, fmt.Errorf("openbaosession: OpenBaoServer %s/%s: %w", workspace, name, errors.Join(errs...))
	}
	if out.Token == "" && (len(out.ClientCert) == 0 || len(out.ClientKey) == 0) {
		return Server{}, fmt.Errorf("%w: %s/%s", ErrNoMaterial, workspace, name)
	}
	return out, nil
}

func pickClient(clients []v1alpha1.OpenBaoServerClientRefs, name string) (v1alpha1.OpenBaoServerClientRefs, bool) {
	if name != "" {
		for _, client := range clients {
			if client.Name == name {
				return client, true
			}
		}
		return v1alpha1.OpenBaoServerClientRefs{}, false
	}
	usable := make([]v1alpha1.OpenBaoServerClientRefs, 0, len(clients))
	for _, client := range clients {
		if client.ClientCert != "" || client.ClientKey != "" {
			usable = append(usable, client)
		}
	}
	if len(usable) == 1 {
		return usable[0], true
	}
	return v1alpha1.OpenBaoServerClientRefs{}, false
}

func fileBytes(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	path := strings.TrimPrefix(value, "file:")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func ParseServer(value, defaultWorkspace string) (string, string) {
	value = strings.TrimSpace(value)
	if defaultWorkspace == "" {
		defaultWorkspace = "root"
	}
	if i := strings.LastIndex(value, "/"); i > 0 {
		return strings.TrimSpace(value[:i]), strings.TrimSpace(value[i+1:])
	}
	if i := strings.LastIndex(value, ":"); i > 0 && !strings.Contains(value, "://") {
		return strings.TrimSpace(value[:i]), strings.TrimSpace(value[i+1:])
	}
	return defaultWorkspace, value
}

type CertificateRequest struct {
	CommonName string

	AltNames []string

	IPSANs []string

	TTL time.Duration
}

type Certificate struct {
	CertificatePEM []byte

	PrivateKeyPEM []byte

	IssuingCAPEM []byte

	CAChainPEM [][]byte

	Serial string
}

type Session struct {
	address string

	ca []byte

	cert []byte

	key []byte

	timeout time.Duration

	client *openbao.Client

	mu sync.Mutex

	pki *openbaoclient.Client

	pkiToken string
}

func Open(ctx context.Context, srv Server) (*Session, error) {
	if strings.TrimSpace(srv.Address) == "" {
		return nil, errors.New("openbaosession: the OpenBaoServer has no address")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := clientOptions(srv)
	if err != nil {
		return nil, err
	}
	token := srv.Token
	var client *openbao.Client
	if token == "" {
		if srv.CertMount == "" || srv.CertRole == "" {
			return nil, errors.New("openbaosession: the OpenBaoServer names no certificate auth mount and role")
		}
		anonymous := openbao.New(srv.Address, "", opts...)
		auth, err := anonymous.LoginCertToken(ctx, srv.CertMount, srv.CertRole)
		if err != nil {
			return nil, fmt.Errorf("openbaosession: logging in to %s at %s/%s: %w", srv.Address, srv.CertMount, srv.CertRole, err)
		}
		token = auth.Token
		client = anonymous.WithToken(token)
	} else {
		client = openbao.New(srv.Address, token, opts...)
	}
	return &Session{
		address: strings.TrimRight(srv.Address, "/"),
		ca:      append([]byte(nil), srv.CACert...),
		cert:    append([]byte(nil), srv.ClientCert...),
		key:     append([]byte(nil), srv.ClientKey...),
		timeout: srv.Timeout,
		client:  client,
	}, nil
}

func clientOptions(srv Server) ([]openbao.Option, error) {
	var opts []openbao.Option
	if len(srv.CACert) > 0 {
		pool, err := openbao.CertPoolFromPEM(srv.CACert)
		if err != nil {
			return nil, fmt.Errorf("openbaosession: the OpenBao CA: %w", err)
		}
		opts = append(opts, openbao.WithCACertPool(pool))
	}
	if len(srv.ClientCert) > 0 || len(srv.ClientKey) > 0 {
		pair, err := tls.X509KeyPair(srv.ClientCert, srv.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("openbaosession: the OpenBao client certificate: %w", err)
		}
		opts = append(opts, openbao.WithClientCertificate(pair))
	}
	return opts, nil
}

func (s *Session) ReadKV(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	kvPath, err := dataPath(mount, path)
	if err != nil {
		return nil, err
	}
	if version <= 0 {
		data, err := s.client.GetKV(ctx, kvPath)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	raw, err := s.client.Read(ctx, fmt.Sprintf("%s?version=%d", kvPath, version))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	data, ok := raw["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openbaosession: %s at version %d did not return a KV v2 data object", kvPath, version)
	}
	return data, nil
}

func (s *Session) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	kvPath, err := dataPath(mount, path)
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	return s.client.PutKV(ctx, kvPath, data)
}

func (s *Session) DeleteKV(ctx context.Context, mount, path string) error {
	kvPath, err := metadataPath(mount, path)
	if err != nil {
		return err
	}
	return s.client.Delete(ctx, kvPath)
}

func (s *Session) IssueCertificate(ctx context.Context, mount, role string, req CertificateRequest) (Certificate, error) {
	if strings.TrimSpace(mount) == "" {
		return Certificate{}, errors.New("openbaosession: issuing a certificate needs a PKI mount")
	}
	if strings.TrimSpace(role) == "" {
		return Certificate{}, errors.New("openbaosession: issuing a certificate needs a PKI role")
	}
	if strings.TrimSpace(req.CommonName) == "" {
		return Certificate{}, errors.New("openbaosession: issuing a certificate needs a common name")
	}
	client, err := s.pkiClient(ctx)
	if err != nil {
		return Certificate{}, err
	}
	request := vault.CertRequest{
		CommonName: req.CommonName,
		AltNames:   append([]string(nil), req.AltNames...),
		IPSANs:     append([]string(nil), req.IPSANs...),
	}
	if req.TTL > 0 {
		request.TTL = req.TTL.String()
	}
	out, err := client.Issue(ctx, "", mount, role, request)
	if err != nil {
		return Certificate{}, err
	}
	certificate := Certificate{
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

func (s *Session) RefreshAt() time.Time {
	return s.client.RefreshAt()
}

func (s *Session) pkiClient(ctx context.Context) (*openbaoclient.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.client.EnsureToken(ctx); err != nil {
		return nil, fmt.Errorf("openbaosession: renewing the OpenBao token: %w", err)
	}
	token := s.client.Token()
	if s.pki != nil && s.pkiToken == token {
		return s.pki, nil
	}
	client, err := openbaoclient.New(openbaoclient.Options{
		Address:    s.address,
		Token:      token,
		CACert:     s.ca,
		ClientCert: s.cert,
		ClientKey:  s.key,
		Timeout:    s.timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("openbaosession: the OpenBao PKI client: %w", err)
	}
	s.pki = client
	s.pkiToken = token
	return client, nil
}

func dataPath(mount, path string) (string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" {
		return "", errors.New("openbaosession: a KV read or write needs a mount")
	}
	if path == "" {
		return "", errors.New("openbaosession: a KV read or write needs a path")
	}
	if strings.HasPrefix(path, mount+"/data/") {
		return path, nil
	}
	return mount + "/data/" + path, nil
}

func metadataPath(mount, path string) (string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" {
		return "", errors.New("openbaosession: a KV delete needs a mount")
	}
	if path == "" {
		return "", errors.New("openbaosession: a KV delete needs a path")
	}
	if strings.HasPrefix(path, mount+"/metadata/") {
		return path, nil
	}
	return mount + "/metadata/" + path, nil
}

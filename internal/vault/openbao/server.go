package openbao

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

var (
	ErrNoServer = errors.New("openbao: no OpenBaoServer was named")

	ErrNoMaterial = errors.New("openbao: the OpenBaoServer reports no client certificate")
)

type Server struct {
	Workspace string

	Name string

	Client string

	Address string

	CACert []byte

	ClientCert []byte

	ClientKey []byte

	CertMount string

	CertRole string

	KVMount string

	Timeout time.Duration
}

func (s Server) fingerprint() string {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(s.Address), s.CACert, s.ClientCert, s.ClientKey, []byte(s.CertMount), []byte(s.CertRole)} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s Server) Options() ([]Option, error) {
	var opts []Option
	if len(s.CACert) > 0 {
		pool, err := CertPoolFromPEM(s.CACert)
		if err != nil {
			return nil, fmt.Errorf("openbao: the OpenBao CA: %w", err)
		}
		opts = append(opts, WithCACertPool(pool))
	}
	if len(s.ClientCert) > 0 || len(s.ClientKey) > 0 {
		pair, err := tls.X509KeyPair(s.ClientCert, s.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("openbao: the OpenBao client certificate: %w", err)
		}
		opts = append(opts, WithClientCertificate(pair))
	}
	if s.Timeout > 0 {
		opts = append(opts, WithTimeout(s.Timeout))
	}
	return opts, nil
}

func Open(ctx context.Context, srv Server) (*Client, error) {
	if strings.TrimSpace(srv.Address) == "" {
		return nil, errors.New("openbao: the OpenBaoServer has no address")
	}
	if srv.CertMount == "" || srv.CertRole == "" {
		return nil, errors.New("openbao: the OpenBaoServer names no certificate auth mount and role")
	}
	opts, err := srv.Options()
	if err != nil {
		return nil, err
	}
	client, err := New(srv.Address, "", opts...).LoginCert(ctx, srv.CertMount, srv.CertRole)
	if err != nil {
		return nil, fmt.Errorf("openbao: logging in to %s at %s/%s: %w", srv.Address, srv.CertMount, srv.CertRole, err)
	}
	return client, nil
}

type Source interface {
	Server(ctx context.Context, workspace, name string) (Server, error)
}

type SourceFunc func(ctx context.Context, workspace, name string) (Server, error)

func (f SourceFunc) Server(ctx context.Context, workspace, name string) (Server, error) {
	return f(ctx, workspace, name)
}

type MountSource interface {
	ServerForMount(ctx context.Context, workspace, name, mount string) (Server, error)
}

func ServerOf(workspace, name string, st v1alpha1.OpenBaoServerStatus, clientName string) (Server, error) {
	if strings.TrimSpace(st.Address) == "" || st.Refs == nil {
		return Server{}, fmt.Errorf("openbao: OpenBaoServer %s/%s reports no address yet (phase %s)", workspace, name, st.Phase)
	}
	out := Server{
		Workspace: workspace,
		Name:      name,
		Address:   strings.TrimRight(strings.TrimSpace(st.Address), "/"),
		CertMount: st.Refs.CertAuthMount,
		CertRole:  st.Refs.CertAuthRole,
		KVMount:   st.Refs.KVMount,
	}
	var errs []error
	var err error
	if out.CACert, err = FileBytes(st.Refs.CA); err != nil {
		errs = append(errs, fmt.Errorf("the OpenBao CA: %w", err))
	}
	client, ok := pickClient(st.Clients, clientName)
	switch {
	case ok:
		out.Client = client.Name
		if out.ClientCert, err = FileBytes(client.ClientCert); err != nil {
			errs = append(errs, fmt.Errorf("client %s certificate: %w", client.Name, err))
		}
		if out.ClientKey, err = FileBytes(client.ClientKey); err != nil {
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
	case clientName != "":
		errs = append(errs, fmt.Errorf("the OpenBaoServer reports no client %q", clientName))
	default:
		if out.ClientCert, err = FileBytes(st.Refs.ClientCert); err != nil {
			errs = append(errs, fmt.Errorf("the OpenBao client certificate: %w", err))
		}
		if out.ClientKey, err = FileBytes(st.Refs.ClientKey); err != nil {
			errs = append(errs, fmt.Errorf("the OpenBao client key: %w", err))
		}
	}
	if len(errs) > 0 {
		return Server{}, fmt.Errorf("openbao: OpenBaoServer %s/%s: %w", workspace, name, errors.Join(errs...))
	}
	if len(out.ClientCert) == 0 || len(out.ClientKey) == 0 {
		return Server{}, fmt.Errorf("%w: %s/%s", ErrNoMaterial, workspace, name)
	}
	return out, nil
}

func ClientForMount(clients []v1alpha1.OpenBaoServerClientRefs, mount string) string {
	mount = strings.Trim(mount, "/")
	if mount == "" {
		return ""
	}
	for _, c := range clients {
		if strings.Trim(c.KVMount, "/") == mount && c.ClientCert != "" {
			return c.Name
		}
	}
	return ""
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

func FileBytes(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	return os.ReadFile(strings.TrimPrefix(value, "file:"))
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

type loginKey struct {
	workspace string

	name string

	client string
}

type login struct {
	fingerprint string

	client *lease.Source[*Client]
}

type Logins struct {
	source Source

	mu sync.Mutex

	clients map[loginKey]login
}

func NewLogins(source Source) *Logins {
	return &Logins{source: source, clients: map[loginKey]login{}}
}

func (l *Logins) Client(ctx context.Context, workspace, name string) (*Client, Server, error) {
	if l == nil || l.source == nil {
		return nil, Server{}, errors.New("openbao: the logins have no OpenBaoServer source")
	}
	srv, err := l.source.Server(ctx, workspace, name)
	if err != nil {
		return nil, Server{}, err
	}
	return l.Open(ctx, srv)
}

func (l *Logins) ClientForMount(ctx context.Context, workspace, name, mount string) (*Client, Server, error) {
	if l == nil || strings.Trim(mount, "/") == "" {
		return l.Client(ctx, workspace, name)
	}
	ms, ok := l.source.(MountSource)
	if !ok {
		return l.Client(ctx, workspace, name)
	}
	srv, err := ms.ServerForMount(ctx, workspace, name, mount)
	if err != nil {
		return nil, Server{}, err
	}
	return l.Open(ctx, srv)
}

func (l *Logins) Open(ctx context.Context, srv Server) (*Client, Server, error) {
	key := loginKey{workspace: srv.Workspace, name: srv.Name, client: srv.Client}
	fp := srv.fingerprint()
	l.mu.Lock()
	entry, ok := l.clients[key]
	if !ok || entry.fingerprint != fp {
		entry = login{fingerprint: fp, client: lease.New(func(ctx context.Context) (*Client, time.Time, error) {
			c, err := Open(ctx, srv)
			return c, time.Time{}, err
		})}
		l.clients[key] = entry
	}
	l.mu.Unlock()
	client, err := entry.client.Get(ctx)
	if err != nil {
		entry.client.Invalidate()
		return nil, srv, err
	}
	return client, srv, nil
}

func (l *Logins) Forget(workspace, name string) {
	l.mu.Lock()
	for k := range l.clients {
		if k.workspace == workspace && k.name == name {
			delete(l.clients, k)
		}
	}
	l.mu.Unlock()
}

func (l *Logins) ForgetWorkspace(workspace string) {
	l.mu.Lock()
	for k := range l.clients {
		if k.workspace == workspace {
			delete(l.clients, k)
		}
	}
	l.mu.Unlock()
}

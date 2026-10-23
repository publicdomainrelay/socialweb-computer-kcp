package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

var ErrUnbound = errors.New("vault: the Vault is not bound to a ready backend")

var VaultGroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: "Vault"}

type Binding struct {
	Vault commonv1.ObjectRef

	Backend commonv1.ObjectRef

	Server *v1alpha1.OpenBaoServerStatus

	Remote *v1alpha1.RemoteOpenBao
}

type bindingContract struct {
	Vault commonv1.ObjectRef

	Backend commonv1.ObjectRef

	Address string

	CABundle string

	Refs *v1alpha1.OpenBaoServerRefs

	Clients []v1alpha1.OpenBaoServerClientRefs

	Remote *v1alpha1.RemoteOpenBaoSpec
}

func (b Binding) fingerprint() string {
	c := bindingContract{Vault: b.Vault, Backend: b.Backend}
	if b.Server != nil {
		c.Address, c.CABundle, c.Refs, c.Clients = b.Server.Address, b.Server.CABundle, b.Server.Refs, b.Server.Clients
	}
	if b.Remote != nil {
		spec := b.Remote.Spec
		c.Remote = &spec
	}
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type vaultKey struct {
	workspace string

	name string
}

func keyOf(ref commonv1.ObjectRef) vaultKey {
	return vaultKey{workspace: strings.TrimSpace(ref.Workspace), name: strings.TrimSpace(ref.Name)}
}

type remoteLogin struct {
	fingerprint string

	client *lease.Source[*openbao.Client]
}

type Provider struct {
	remote *openbao.Remote

	logins *openbao.Logins

	mu sync.RWMutex

	bindings map[vaultKey]Binding

	remotes map[vaultKey]remoteLogin
}

func NewProvider(remote *openbao.Remote) *Provider {
	p := &Provider{remote: remote, bindings: map[vaultKey]Binding{}, remotes: map[vaultKey]remoteLogin{}}
	p.logins = openbao.NewLogins(openbao.SourceFunc(func(context.Context, string, string) (openbao.Server, error) {
		return openbao.Server{}, errors.New("vault: the provider opens servers from its bindings")
	}))
	return p
}

func (p *Provider) Bind(b Binding) bool {
	key := keyOf(b.Vault)
	p.mu.Lock()
	defer p.mu.Unlock()
	old, ok := p.bindings[key]
	if ok && old.fingerprint() == b.fingerprint() {
		return false
	}
	p.bindings[key] = b
	delete(p.remotes, key)
	if ok && old.Backend.Name != "" {
		p.logins.Forget(old.Backend.Workspace, old.Backend.Name)
	}
	return true
}

func (p *Provider) Unbind(ref commonv1.ObjectRef) {
	key := keyOf(ref)
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.bindings[key]; ok && old.Backend.Name != "" {
		p.logins.Forget(old.Backend.Workspace, old.Backend.Name)
	}
	delete(p.bindings, key)
	delete(p.remotes, key)
}

func (p *Provider) binding(ref commonv1.ObjectRef) (Binding, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	b, ok := p.bindings[keyOf(ref)]
	return b, ok
}

func (p *Provider) clientFor(to commonv1.ObjectRef) openbao.ClientFor {
	return func(ctx context.Context, mount string) (*openbao.Client, error) {
		b, ok := p.binding(to)
		if !ok {
			return nil, fmt.Errorf("%w: %s/%s", ErrUnbound, to.Workspace, to.Name)
		}
		switch {
		case b.Server != nil:
			srv, err := openbao.ServerOf(b.Backend.Workspace, b.Backend.Name, *b.Server, clientName(b.Server.Clients, mount))
			if err != nil {
				return nil, err
			}
			client, _, err := p.logins.Open(ctx, srv)
			return client, err
		case b.Remote != nil:
			return p.remoteClient(ctx, keyOf(to), b)
		default:
			return nil, fmt.Errorf("%w: %s/%s", ErrUnbound, to.Workspace, to.Name)
		}
	}
}

func clientName(clients []v1alpha1.OpenBaoServerClientRefs, mount string) string {
	if name := openbao.ClientForMount(clients, mount); name != "" || strings.Trim(mount, "/") != "" {
		return name
	}
	name := ""
	for _, c := range clients {
		if c.ClientCert == "" || strings.TrimSpace(c.KVMount) != "" {
			continue
		}
		if name != "" {
			return ""
		}
		name = c.Name
	}
	return name
}

func (p *Provider) remoteClient(ctx context.Context, key vaultKey, b Binding) (*openbao.Client, error) {
	fp := b.fingerprint()
	p.mu.Lock()
	entry, ok := p.remotes[key]
	if !ok || entry.fingerprint != fp {
		remote := b.Remote.DeepCopy()
		entry = remoteLogin{fingerprint: fp, client: lease.New(func(ctx context.Context) (*openbao.Client, time.Time, error) {
			c, err := p.remote.Open(ctx, b.Backend.Workspace, remote)
			return c, time.Time{}, err
		})}
		p.remotes[key] = entry
	}
	p.mu.Unlock()
	return entry.client.Get(ctx)
}

func (p *Provider) defaultMount(to commonv1.ObjectRef) string {
	b, ok := p.binding(to)
	if !ok {
		return ""
	}
	if b.Server != nil && b.Server.Refs != nil {
		return b.Server.Refs.KVMount
	}
	if b.Remote != nil {
		return b.Remote.Spec.Mounts.KV
	}
	return ""
}

func (p *Provider) open(_ context.Context, to commonv1.ObjectRef, scope []string) (*openbao.Handle, error) {
	if _, ok := p.binding(to); !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnbound, to.Workspace, to.Name)
	}
	return openbao.NewHandle(p.clientFor(to), p.defaultMount(to), scope)
}

func (p *Provider) Register(b *bus.Bus) {
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityRead, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (contractvault.KV, error) {
		h, err := p.open(ctx, to, scope)
		if err != nil {
			return nil, err
		}
		return readOnly{h}, nil
	})
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityWrite, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (contractvault.KV, error) {
		return p.open(ctx, to, scope)
	})
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityLease, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (contractvault.Leases, error) {
		return p.open(ctx, to, scope)
	})
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityWrite, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (contractvault.Keys, error) {
		return p.open(ctx, to, scope)
	})
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityPKIIssue, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (contractvault.PKI, error) {
		return p.open(ctx, to, scope)
	})
}

var ErrReadOnly = errors.New("vault: the handle was granted vault.read only")

var _ contractvault.KV = readOnly{}

type readOnly struct {
	h *openbao.Handle
}

func (r readOnly) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	return r.h.Read(ctx, mount, path, version)
}

func (readOnly) Write(context.Context, string, string, map[string]any) error { return ErrReadOnly }

func (readOnly) Delete(context.Context, string, string) error { return ErrReadOnly }

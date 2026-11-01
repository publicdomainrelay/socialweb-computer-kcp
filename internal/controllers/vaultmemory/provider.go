package vaultmemory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const (
	DefaultKVMount = "secret"

	Address = "memory://vault"

	Version = "memory"
)

var (
	ErrUnbound = errors.New("vaultmemory: the Vault is not bound to a ready local store")

	ErrOutOfScope = errors.New("vaultmemory: the path is outside the handle's scope")

	ErrReadOnly = errors.New("vaultmemory: the handle was granted vault.read only")
)

var VaultGroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: "Vault"}

type vaultKey struct {
	workspace string

	name string
}

func keyOf(ref commonv1.ObjectRef) vaultKey {
	return vaultKey{workspace: strings.TrimSpace(ref.Workspace), name: strings.TrimSpace(ref.Name)}
}

type Binding struct {
	Vault commonv1.ObjectRef

	Mount string
}

type Provider struct {
	mu sync.RWMutex

	bindings map[vaultKey]Binding

	stores map[string]*Store

	onChange func()
}

func NewProvider() *Provider {
	return &Provider{bindings: map[vaultKey]Binding{}, stores: map[string]*Store{}}
}

func (p *Provider) Bind(b Binding) bool {
	if strings.TrimSpace(b.Vault.Name) == "" || strings.TrimSpace(b.Vault.Workspace) == "" {
		return false
	}
	b.Mount = strings.Trim(strings.TrimSpace(b.Mount), "/")
	if b.Mount == "" {
		b.Mount = DefaultKVMount
	}
	k := keyOf(b.Vault)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings == nil {
		p.bindings = map[vaultKey]Binding{}
	}
	if old, ok := p.bindings[k]; ok && old.Mount == b.Mount {
		return false
	}
	p.bindings[k] = b
	return true
}

func (p *Provider) Unbind(ref commonv1.ObjectRef) {
	p.mu.Lock()
	delete(p.bindings, keyOf(ref))
	p.mu.Unlock()
}

func (p *Provider) Store(workspace string) *Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.storeLocked(strings.TrimSpace(workspace))
}

func (p *Provider) storeLocked(workspace string) *Store {
	if p.stores == nil {
		p.stores = map[string]*Store{}
	}
	store := p.stores[workspace]
	if store == nil {
		store = NewStore()
		store.SetOnChange(p.onChange)
		p.stores[workspace] = store
	}
	return store
}

func (p *Provider) SetOnChange(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = fn
	for _, store := range p.stores {
		store.SetOnChange(fn)
	}
}

func (p *Provider) binding(ref commonv1.ObjectRef) (Binding, *Store, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.bindings[keyOf(ref)]
	if !ok {
		return Binding{}, nil, false
	}
	return b, p.storeLocked(strings.TrimSpace(ref.Workspace)), true
}

func (p *Provider) open(to commonv1.ObjectRef, scope []string) (*handle, error) {
	b, store, ok := p.binding(to)
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnbound, to.Workspace, to.Name)
	}
	cleaned := cleanScope(scope)
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("%w: a handle needs a scope", ErrOutOfScope)
	}
	return &handle{store: store, mount: b.Mount, scope: cleaned}, nil
}

func (p *Provider) Register(b *bus.Bus) {
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityRead, func(_ context.Context, to commonv1.ObjectRef, scope []string) (contractvault.KV, error) {
		h, err := p.open(to, scope)
		if err != nil {
			return nil, err
		}
		return readOnly{h}, nil
	})
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityWrite, func(_ context.Context, to commonv1.ObjectRef, scope []string) (contractvault.KV, error) {
		return p.open(to, scope)
	})
}

type Snapshot struct {
	Workspaces map[string]StoreSnapshot `json:"workspaces"`
}

func (p *Provider) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := Snapshot{Workspaces: make(map[string]StoreSnapshot, len(p.stores))}
	for workspace, store := range p.stores {
		out.Workspaces[workspace] = store.Snapshot()
	}
	return out
}

func (p *Provider) Restore(snapshot Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stores == nil {
		p.stores = map[string]*Store{}
	}
	for workspace, store := range p.stores {
		store.Restore(snapshot.Workspaces[workspace])
	}
	for workspace, s := range snapshot.Workspaces {
		if _, ok := p.stores[workspace]; ok {
			continue
		}
		store := NewStore()
		store.SetOnChange(p.onChange)
		store.Restore(s)
		p.stores[workspace] = store
	}
}

type handle struct {
	store *Store

	mount string

	scope []string
}

var _ contractvault.KV = (*handle)(nil)

func (h *handle) target(mount, path string) (string, string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	if mount == "" {
		mount = h.mount
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimPrefix(strings.TrimPrefix(path, mount+"/data/"), mount+"/metadata/")
	if mount == "" || path == "" {
		return "", "", ErrInvalidRef
	}
	if !allows(h.scope, mount+"/"+path) {
		return "", "", fmt.Errorf("%w: %s/%s", ErrOutOfScope, mount, path)
	}
	return mount, path, nil
}

func (h *handle) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	mount, path, err := h.target(mount, path)
	if err != nil {
		return nil, err
	}
	return h.store.Read(ctx, mount, path, version)
}

func (h *handle) Write(ctx context.Context, mount, path string, data map[string]any) error {
	mount, path, err := h.target(mount, path)
	if err != nil {
		return err
	}
	return h.store.Write(ctx, mount, path, data)
}

func (h *handle) Delete(ctx context.Context, mount, path string) error {
	mount, path, err := h.target(mount, path)
	if err != nil {
		return err
	}
	return h.store.Delete(ctx, mount, path)
}

var _ contractvault.KV = readOnly{}

type readOnly struct {
	*handle
}

func (readOnly) Write(context.Context, string, string, map[string]any) error { return ErrReadOnly }

func (readOnly) Delete(context.Context, string, string) error { return ErrReadOnly }

func cleanScope(scope []string) []string {
	out := make([]string, 0, len(scope))
	for _, s := range scope {
		if s = strings.Trim(strings.TrimSpace(s), "/"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func allows(scope []string, target string) bool {
	target = strings.Trim(target, "/")
	for _, s := range scope {
		if s == "*" || s == target {
			return true
		}
		if prefix, ok := strings.CutSuffix(s, "*"); ok && strings.HasPrefix(target, prefix) {
			return true
		}
		if strings.HasPrefix(target, s+"/") {
			return true
		}
	}
	return false
}

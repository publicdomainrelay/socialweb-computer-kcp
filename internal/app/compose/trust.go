package compose

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const TrustScope = vault.TrustScope

var ErrNoTrust = errors.New("compose: the root publishes no CA bundle yet")

type trustKey struct {
	workspace string

	kind string

	name string
}

func trustKeyOf(ref commonv1.ObjectRef) trustKey {
	return trustKey{workspace: strings.TrimSpace(ref.Workspace), kind: strings.ToLower(strings.TrimSpace(ref.Kind)), name: strings.TrimSpace(ref.Name)}
}

type trustEntry struct {
	bundle []byte

	watchers map[uint64]func([]byte)
}

type Trusts struct {
	mu sync.Mutex

	entries map[trustKey]*trustEntry

	next uint64
}

func (t *Trusts) entry(k trustKey) *trustEntry {
	if t.entries == nil {
		t.entries = map[trustKey]*trustEntry{}
	}
	e, ok := t.entries[k]
	if !ok {
		e = &trustEntry{watchers: map[uint64]func([]byte){}}
		t.entries[k] = e
	}
	return e
}

func (t *Trusts) Publish(ref commonv1.ObjectRef, bundle []byte) {
	t.mu.Lock()
	e := t.entry(trustKeyOf(ref))
	if bytes.Equal(e.bundle, bundle) {
		t.mu.Unlock()
		return
	}
	e.bundle = slices.Clone(bundle)
	watchers := make([]func([]byte), 0, len(e.watchers))
	for _, fn := range e.watchers {
		watchers = append(watchers, fn)
	}
	t.mu.Unlock()
	for _, fn := range watchers {
		fn(slices.Clone(bundle))
	}
}

func (t *Trusts) Provide(b *bus.Bus, gk schema.GroupKind) {
	bus.Provide(b, gk, vault.CapabilityRead, func(_ context.Context, to commonv1.ObjectRef, scope []string) (vault.Trust, error) {
		if !slices.Contains(scope, TrustScope) && !slices.Contains(scope, ScopeAll) {
			return nil, bus.ErrLinkDenied
		}
		return trustHandle{t: t, key: trustKeyOf(to)}, nil
	})
}

var _ vault.Trust = trustHandle{}

type trustHandle struct {
	t *Trusts

	key trustKey
}

func (h trustHandle) CABundle(context.Context) ([]byte, error) {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	e := h.t.entry(h.key)
	if len(e.bundle) == 0 {
		return nil, ErrNoTrust
	}
	return slices.Clone(e.bundle), nil
}

func (h trustHandle) Watch(fn func([]byte)) func() {
	if fn == nil {
		return func() {}
	}
	h.t.mu.Lock()
	h.t.next++
	id := h.t.next
	h.t.entry(h.key).watchers[id] = fn
	h.t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.t.mu.Lock()
			delete(h.t.entry(h.key).watchers, id)
			h.t.mu.Unlock()
		})
	}
}

func (c *Composer) PublishNetworkTrust(r Root, network *v1alpha1.MarketNetworkValue) *commonv1.ObjectRef {
	if network == nil || strings.TrimSpace(network.CABundle) == "" {
		return nil
	}
	ref := r.Ref()
	c.PublishTrust(ref, []byte(strings.TrimSpace(network.CABundle)+"\n"))
	return &ref
}

func (c *Composer) NetworkComponentSpec(r Root, stateDir string, network *v1alpha1.MarketNetworkValue) v1alpha1.ComponentSpec {
	spec := NewComponentSpec(stateDir, network)
	if ca := c.PublishNetworkTrust(r, network); ca != nil {
		spec.TLS = &v1alpha1.ComponentTLS{CARef: ca}
	}
	return spec
}

func TrustLink(from, publisher commonv1.ObjectRef) Link {
	return RefLink(from, publisher, Cap(vault.CapabilityRead, TrustScope))
}

func TrustLinks(ws string, desired []Desired) []Link {
	var out []Link
	for _, d := range desired {
		value, err := SpecMap(d.Spec)
		if err != nil || value == nil {
			continue
		}
		tls, _ := value["tls"].(map[string]any)
		ref, _ := tls["caRef"].(map[string]any)
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		if kind == "" || name == "" {
			continue
		}
		group, _ := ref["group"].(string)
		target, _ := ref["workspace"].(string)
		out = append(out, TrustLink(commonv1.ObjectRef{Workspace: ws, Kind: d.Kind, Name: d.Name},
			commonv1.ObjectRef{Group: group, Kind: kind, Name: name, Workspace: workspaceOr(target, ws)}))
	}
	return out
}

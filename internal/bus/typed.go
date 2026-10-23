package bus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

var ErrNoProvider = errors.New("bus: no provider serves this kind and capability")

type providerKey struct {
	kind string

	capability string

	typ reflect.Type
}

type provider struct {
	group string

	open any
}

func Provide[T any](b *Bus, gk schema.GroupKind, capability string, open func(ctx context.Context, to commonv1.ObjectRef, scope []string) (T, error)) {
	if b == nil || open == nil || strings.TrimSpace(gk.Kind) == "" || strings.TrimSpace(capability) == "" {
		return
	}
	key := providerKey{kind: strings.ToLower(strings.TrimSpace(gk.Kind)), capability: strings.ToLower(strings.TrimSpace(capability)), typ: reflect.TypeFor[T]()}
	b.mu.Lock()
	if b.providers == nil {
		b.providers = map[providerKey]provider{}
	}
	b.providers[key] = provider{group: strings.TrimSpace(gk.Group), open: open}
	b.mu.Unlock()
}

func (b *Bus) Provides(group, kind string) bool {
	if b == nil {
		return false
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	b.mu.RLock()
	defer b.mu.RUnlock()
	for key, p := range b.providers {
		if key.kind == kind && groupMatches(p.group, group) {
			return true
		}
	}
	return false
}

func Resolve[T any](ctx context.Context, b *Bus, from commonv1.ObjectRef, to commonv1.ObjectRef, c Capability) (T, error) {
	var zero T
	if b == nil {
		return zero, ErrHandlersStopped
	}
	if err := b.Ready(ctx); err != nil {
		return zero, err
	}
	caller := Caller{Ref: from}
	if current, ok := CallerFrom(ctx); ok && current.Ref == from {
		caller.Labels = current.Labels
	}
	if err := b.authorizeCaller(caller, to, c); err != nil {
		return zero, err
	}
	key := providerKey{kind: strings.ToLower(strings.TrimSpace(to.Kind)), capability: strings.ToLower(strings.TrimSpace(c.Name)), typ: reflect.TypeFor[T]()}
	b.mu.RLock()
	p, ok := b.providers[key]
	b.mu.RUnlock()
	if !ok || !groupMatches(p.group, to.Group) {
		return zero, fmt.Errorf("%w: %s for %s as %v", ErrNoProvider, c.Name, refString(to), key.typ)
	}
	open, ok := p.open.(func(context.Context, commonv1.ObjectRef, []string) (T, error))
	if !ok {
		return zero, fmt.Errorf("%w: %s for %s as %v", ErrNoProvider, c.Name, refString(to), key.typ)
	}
	return open(context.WithValue(ctx, callerKey{}, caller), to, append([]string(nil), c.Scope...))
}

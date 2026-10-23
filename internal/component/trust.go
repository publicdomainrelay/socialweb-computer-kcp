package component

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
)

type Trust struct {
	ref *commonv1.ObjectRef

	pool *tlsreload.Pool
}

func NewTrust(caRef *commonv1.ObjectRef, log *slog.Logger) *Trust {
	if caRef == nil || caRef.IsZero() {
		return &Trust{}
	}
	ref := *caRef
	return &Trust{ref: &ref, pool: tlsreload.EmptyPool(log)}
}

func (t *Trust) Enabled() bool {
	return t != nil && t.ref != nil
}

func (t *Trust) Client(base *tls.Config) *tls.Config {
	if !t.Enabled() {
		return base
	}
	return t.pool.ClientConfig(base)
}

func (t *Trust) Follow(ctx context.Context, in Instance) (func(), error) {
	if !t.Enabled() {
		return func() {}, nil
	}
	if in.Bus == nil {
		return nil, errors.New("component: the CA ref needs the bus")
	}
	to := *t.ref
	to.Workspace = to.InWorkspace(in.Self.Workspace)
	trust, err := bus.Resolve[vault.Trust](in.Caller(ctx), in.Bus, in.Self, to,
		bus.Capability{Name: vault.CapabilityRead, Scope: []string{vault.TrustScope}})
	if err != nil {
		return nil, fmt.Errorf("component: the CA bundle of %s: %w", to.String(), err)
	}
	return t.pool.Follow(ctx, trust), nil
}

package vault

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

var caKeys = []string{"caBundle", "ca.crt", "ca", "certificate", "value"}

type Roots struct {
	bus *bus.Bus

	log *slog.Logger

	pools tlsreload.Pools
}

func NewRoots(b *bus.Bus, log *slog.Logger) *Roots {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Roots{bus: b, log: log}
}

func (r *Roots) Pool(ctx context.Context, from commonv1.ObjectRef, caRef *commonv1.ObjectRef, caSecret *commonv1.SecretRef) (*tlsreload.Pool, error) {
	if caRef != nil && caRef.IsZero() {
		caRef = nil
	}
	switch {
	case caRef == nil && caSecret == nil:
		return nil, nil
	case caRef != nil && caSecret != nil:
		return nil, errors.New("vault: tls names both caRef and caSecretRef")
	case r == nil || r.bus == nil:
		return nil, errors.New("vault: the CA refs need the bus")
	}
	fingerprint := ""
	if caRef != nil {
		fingerprint = "trust:" + caRef.String()
	} else {
		to, _, _, err := secretTarget(from, *caSecret)
		if err != nil {
			return nil, err
		}
		fingerprint = "secret:" + to.String() + "/" + SecretScope(*caSecret) + "#" + caSecret.Key
	}
	return r.pools.Get(from.String(), fingerprint, func() (*tlsreload.Pool, func(), error) {
		if caRef != nil {
			to := *caRef
			to.Workspace = to.InWorkspace(from.Workspace)
			trust, err := bus.Resolve[contractvault.Trust](ctx, r.bus, from, to,
				bus.Capability{Name: contractvault.CapabilityRead, Scope: []string{contractvault.TrustScope}})
			if err != nil {
				return nil, nil, fmt.Errorf("vault: the CA bundle of %s for %s: %w", to.String(), from.String(), err)
			}
			pool, stop := tlsreload.TrustPool(ctx, trust, r.log.With("ca", to.String()))
			return pool, stop, nil
		}
		ref := *caSecret
		pool := tlsreload.SecretPool(func(ctx context.Context) ([]byte, error) {
			data, err := ReadSecret(ctx, r.bus, from, ref)
			if err != nil {
				return nil, err
			}
			value := SecretValue(data, ref.Key, caKeys...)
			if value == "" {
				return nil, fmt.Errorf("vault: the CA secret %s holds no CA bundle", SecretScope(ref))
			}
			return []byte(value), nil
		}, SecretRenew, r.log)
		return pool, func() {}, nil
	})
}

func (r *Roots) Drop(from commonv1.ObjectRef) {
	if r != nil {
		r.pools.Drop(from.String())
	}
}

func Remote(b *bus.Bus, log *slog.Logger) *openbao.Remote {
	roots := NewRoots(b, log)
	return &openbao.Remote{Enabled: true, Roots: roots.Pool, Release: roots.Drop,
		Secrets: func(ctx context.Context, from commonv1.ObjectRef, ref commonv1.SecretRef) (map[string]any, error) {
			return ReadSecret(ctx, b, from, ref)
		}}
}

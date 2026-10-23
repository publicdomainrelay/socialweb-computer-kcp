package providermain

import (
	"context"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
)

func newRoller(units *clusterprovider.UnitsHost, stack clusterprovider.StackConfig, registry clusterprovider.Clusters,
	alloc *portallocator.Allocator, purposes []portallocator.Purpose, name string, log *slog.Logger) *clusterprovider.Roller {
	refs := func(ctx context.Context) (map[string]clusterprovider.Ref, error) {
		list, err := registry.Refs(ctx)
		if err != nil {
			return nil, err
		}
		out := map[string]clusterprovider.Ref{}
		for _, r := range list {
			if key, err := r.Key(); err == nil {
				out[key] = r
			}
		}
		return out, nil
	}
	return &clusterprovider.Roller{
		Units: units,
		Stack: stack,
		Clusters: func(ctx context.Context) ([]string, error) {
			m, err := refs(ctx)
			if err != nil {
				return nil, err
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			return keys, nil
		},
		Ready: func(ctx context.Context, cluster string) (bool, error) {
			m, err := refs(ctx)
			if err != nil {
				return false, err
			}
			ref, ok := m[cluster]
			if !ok {
				return false, nil
			}
			cl, err := registry.Read(ctx, ref)
			if err != nil {
				return false, err
			}
			return cl != nil && cl.Status.Phase == v1alpha1.ClusterReady, nil
		},
		Leases: func(cluster string) (materialiser.Leases, error) {
			out := materialiser.Leases{}
			for _, p := range purposes {
				if l, ok := alloc.PortFor(cluster, p); ok {
					out[p] = l
				}
			}
			return out, nil
		},
		Relet: func(ctx context.Context, cluster string, p portallocator.Purpose) (portallocator.Lease, bool, error) {
			return alloc.Relet(ctx, portallocator.Request{Cluster: cluster, Purpose: p})
		},
		Name: name,
		Log:  log,
	}
}

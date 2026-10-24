package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tenantstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

var ErrNoRevokedEraser = errors.New(
	"clusterprovider: the identity that owns this cluster is revoked and no datastore eraser is wired, " +
		"so its ServiceAccount would be served by k3s from its first answer: refusing to start k3s")

type RevokedServiceAccounts struct {
	State func(ctx context.Context, did string) (v1alpha1.IdentityState, error)

	Account func(did string) tenantstore.ServiceAccount

	Store tenantstore.Eraser

	Log *slog.Logger
}

func (r *RevokedServiceAccounts) beforeK3s(steps []procgroup.Step, o materialiser.Observed, spec clusterstack.Spec) []procgroup.Step {
	did := o.Cluster.Spec.Account
	if r == nil || did == "" {
		return steps
	}
	for i := range steps {
		if steps[i].Name != procgroup.ComponentK3s {
			continue
		}
		inner := steps[i].Prepare
		steps[i].Prepare = func(ctx context.Context, ps procgroup.Spec) (procgroup.Spec, error) {
			if err := r.erase(ctx, spec, did, o.IdentityState); err != nil {
				return ps, err
			}
			if inner == nil {
				return ps, nil
			}
			return inner(ctx, ps)
		}
	}
	return steps
}

func (r *RevokedServiceAccounts) erase(ctx context.Context, spec clusterstack.Spec, did string, observed v1alpha1.IdentityState) error {
	state := observed
	if r.State != nil {
		fresh, err := r.State(ctx, did)
		if err != nil {
			return fmt.Errorf("clusterprovider: reading whether the identity of %s is revoked before k3s starts: %w", spec.Cluster, err)
		}
		if revocation.Revoked(fresh) {
			state = fresh
		}
	}
	if !revocation.Revoked(state) {
		return nil
	}
	if r.Store == nil || r.Account == nil {
		return fmt.Errorf("%w (cluster %s, identity %s)", ErrNoRevokedEraser, spec.Cluster, state)
	}
	sa := r.Account(did)
	start := time.Now()
	erased, err := r.Store.EraseServiceAccount(ctx, spec.DatastoreEndpoint, sa)
	if err != nil {
		return fmt.Errorf("clusterprovider: erasing the ServiceAccount %s/%s of revoked identity %s from the datastore of %s before k3s starts: %w",
			sa.Namespace, sa.Name, did, spec.Cluster, err)
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("cluster provider: erased the ServiceAccount of a revoked identity from the datastore before k3s starts",
		"cluster", spec.Cluster, "did", did, "state", string(state), "serviceAccount", sa.Namespace+"/"+sa.Name,
		"deleted", erased.ServiceAccount, "tokenSecrets", erased.Secrets, "datastore", spec.DatastoreEndpoint,
		"slot", spec.Slot, "took", time.Since(start).String())
	return nil
}

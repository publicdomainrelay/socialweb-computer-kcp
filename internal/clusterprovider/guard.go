package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/identitybundle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/instance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

var (
	ErrNoRowDeleter = errors.New(
		"clusterprovider: the datastore holds a bootstrap row encrypted with the previous " +
			"instance's token and no row deleter is wired, so k3s would exit immediately; " +
			"see internal/instance")
	ErrNoBundleSeeder = errors.New(
		"clusterprovider: the identity bundle must be pre-seeded before k3s starts and no " +
			"seeder is wired; see internal/identitybundle")
	ErrNoBundle = errors.New(
		"clusterprovider: no identity bundle source was supplied, so the pre-seeded entries " +
			"cannot be verified; an unverified bundle is what GuardFor exists to refuse")
	ErrNoTokenSource = errors.New(
		"clusterprovider: no token source was supplied, so instance.PlanFor cannot answer; the " +
			"token plan is not optional because a k3s started with the wrong token dies on the " +
			"next start with 'bootstrap data already found and encrypted with different token'")
)

type GuardDeps struct {
	Bundle func(cluster string) (*identitybundle.Bundle, error)

	StateDir string

	Token func(cluster string) instance.Token

	HasRow func(ctx context.Context, cluster string) bool

	DeleteRow func(ctx context.Context, cluster string) error

	SeedBundle func(ctx context.Context, cluster string) error

	Log *slog.Logger
}

func GuardFor(d GuardDeps) materialiser.IdentityGuard {
	return func(ctx context.Context, o materialiser.Observed) error {
		cluster := o.Cluster.Name
		log := d.Log
		if log == nil {
			log = slog.Default()
		}
		dir, err := ClusterDataDir(d.StateDir, cluster)
		if err != nil {
			return err
		}
		if d.Token == nil {
			return fmt.Errorf("%w (cluster %s)", ErrNoTokenSource, cluster)
		}
		if d.Bundle == nil {
			return fmt.Errorf("%w (cluster %s)", ErrNoBundle, cluster)
		}

		if uid := string(o.Cluster.UID); uid != "" {
			bdir, err := BundleDir(d.StateDir, cluster)
			if err != nil {
				return err
			}
			adopted, err := identitybundle.BindClusterUID(bdir, uid)
			if err != nil {
				return fmt.Errorf("clusterprovider: %s: refusing to seed or verify it: %w", cluster, err)
			}
			if adopted {
				log.Info("guard: identity bundle adopted for uid", "cluster", cluster, "uid", uid)
			}
		}

		plan, err := instance.PlanFor(d.Token(cluster), d.HasRow != nil && d.HasRow(ctx, cluster))
		if err != nil {
			return fmt.Errorf("clusterprovider: the token plan for %s: %w", cluster, err)
		}
		log.Info("guard: token plan", "cluster", cluster, "dataDir", dir,
			"deleteBootstrapRow", plan.DeleteBootstrapRow, "reSeedBundle", plan.ReSeedBundle,
			"reason", plan.Reason)

		if plan.DeleteBootstrapRow {
			if d.DeleteRow == nil {
				return fmt.Errorf("%w (cluster %s)", ErrNoRowDeleter, cluster)
			}
			if err := d.DeleteRow(ctx, cluster); err != nil {
				return fmt.Errorf("clusterprovider: deleting the bootstrap row for %s: %w", cluster, err)
			}
		}
		if plan.ReSeedBundle {
			if d.SeedBundle == nil {
				return fmt.Errorf("%w (cluster %s)", ErrNoBundleSeeder, cluster)
			}
			if err := d.SeedBundle(ctx, cluster); err != nil && !errors.Is(err, ErrNoBundleYet) {
				return fmt.Errorf("clusterprovider: seeding the identity bundle for %s: %w", cluster, err)
			}
		}

		b, err := d.Bundle(cluster)
		if errors.Is(err, ErrNoBundleYet) {
			log.Warn("guard: no identity bundle is persisted for this cluster yet, so there is "+
				"nothing to verify. k3s authors the 13 entries on a datastore with no bootstrap row, "+
				"and the harvest after the group is up is what persists them; every later start "+
				"seeds from that copy and verifies it byte for byte",
				"cluster", cluster, "bundleDir", dir)
			return nil
		}
		if err != nil {
			return err
		}
		root, err := bundleRoot(d.StateDir, cluster)
		if err != nil {
			return err
		}
		return b.Verify(root)
	}
}

var ErrNoHarvestSeam = errors.New(
	"clusterprovider: this Stack puts no k3s step in the group, so there is no moment after " +
		"the CAs exist and before anything else could need them, and the harvest would SILENTLY " +
		"NOT RUN -- a bundle harvested by nothing and a bundle harvested correctly look " +
		"identical from a green suite")

func HarvestAfterK3s(steps []procgroup.Step, harvest func(ctx context.Context, cluster string) error, cluster string) ([]procgroup.Step, error) {
	if harvest == nil {
		return nil, fmt.Errorf(
			"clusterprovider: this Stack has a k3s step and no identity-bundle harvest, so the " +
				"cluster's CAs would never be persisted and every rebuild would be a NEW cluster " +
				"under the same name -- invalidating every kubeconfig issued before it")
	}
	idx := -1
	for i, s := range steps {
		if s.Name == procgroup.ComponentK3s {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrNoHarvestSeam
	}
	inner := steps[idx].Ready
	steps[idx].Ready = func(ctx context.Context, p procgroup.Member) error {
		if inner != nil {
			if err := inner(ctx, p); err != nil {
				return err
			}
		}
		return harvest(ctx, cluster)
	}
	return steps, nil
}

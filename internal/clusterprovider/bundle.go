package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/identitybundle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const bundleDirName = "identity-bundle"

var ErrNoBundleYet = errors.New(
	"clusterprovider: this cluster has no persisted identity bundle, so there is nothing to " +
		"pre-seed and nothing to verify. k3s authors the 13 entries itself on a datastore with no " +
		"bootstrap row, and the harvest after the group is up is what persists them; from then on " +
		"every start seeds and verifies")

func BundleDir(stateDir, cluster string) (string, error) {
	dataDir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, bundleDirName), nil
}

func bundleRoot(stateDir, cluster string) (string, error) {
	dataDir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "server"), nil
}

func PersistedBundle(stateDir, cluster string) (*identitybundle.Bundle, error) {
	dir, err := BundleDir(stateDir, cluster)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, identitybundle.Paths[0])); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoBundleYet
		}
		return nil, fmt.Errorf("clusterprovider: reading the persisted identity bundle for %s: %w", cluster, err)
	}
	b, err := identitybundle.Harvest(dir)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: the persisted identity bundle for %s at %s is "+
			"unusable, and seeding a partial one makes k3s REGENERATE the missing CAs and "+
			"INVALIDATE every kubeconfig and ServiceAccount token: %w", cluster, dir, err)
	}
	return b, nil
}

func SeedBundle(stateDir, cluster string) error {
	b, err := PersistedBundle(stateDir, cluster)
	if err != nil {
		return err
	}
	root, err := bundleRoot(stateDir, cluster)
	if err != nil {
		return err
	}
	if err := b.Seed(root); err != nil {
		return fmt.Errorf("clusterprovider: seeding the identity bundle for %s into %s: %w", cluster, root, err)
	}
	return nil
}

func SeedBundleForWake(ctx context.Context, logins *openbao.Logins, stateDir, cluster string) error {
	if err := SeedBundleFromVault(ctx, logins, stateDir, cluster); err == nil {
		return nil
	} else if !errors.Is(err, ErrNoVaultBundle) {
		return err
	}
	if err := SeedBundle(stateDir, cluster); err == nil {
		return nil
	} else if !errors.Is(err, ErrNoBundleYet) {
		return err
	}
	return ErrNoBundleYet
}

func HarvestBundle(stateDir, cluster string) error {
	root, err := bundleRoot(stateDir, cluster)
	if err != nil {
		return err
	}
	b, err := identitybundle.Harvest(root)
	if err != nil {
		return fmt.Errorf("clusterprovider: harvesting the identity bundle for %s from %s, which "+
			"k3s authors and this is the only moment it is complete: %w", cluster, root, err)
	}
	dir, err := BundleDir(stateDir, cluster)
	if err != nil {
		return err
	}
	if err := b.Seed(dir); err != nil {
		return fmt.Errorf("clusterprovider: persisting the identity bundle for %s to %s: %w", cluster, dir, err)
	}
	return nil
}

func BundleGuard(stateDir string) func(cluster string) (*identitybundle.Bundle, error) {
	return func(cluster string) (*identitybundle.Bundle, error) {
		return PersistedBundle(stateDir, cluster)
	}
}

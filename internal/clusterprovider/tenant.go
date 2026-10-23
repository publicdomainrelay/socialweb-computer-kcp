package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var ErrNoBootstrap = errors.New("clusterprovider: the tenant bootstrap needs a tenant credential and a grant path")

func (p *Provider) Bootstrap(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) error {
	if p.opts.Tenant == nil || p.opts.GrantPath == "" || p.opts.Grant == nil {
		return fmt.Errorf("%w (cluster %s)", ErrNoBootstrap, ref.Name)
	}
	key, err := ref.Key()
	if err != nil {
		return err
	}
	cred, err := p.opts.Tenant(ctx, ref, cl)
	if err != nil {
		return fmt.Errorf("clusterprovider: no tenant credential for %s: %w", ref.Name, err)
	}
	if cred == nil {
		return fmt.Errorf("clusterprovider: no tenant credential for %s", ref.Name)
	}

	raw, err := os.ReadFile(p.opts.GrantPath)
	if err != nil {
		return fmt.Errorf("clusterprovider: reading the tenant grant: %w", err)
	}
	objects, err := p.opts.Grant(ctx, ref, cred, raw)
	if err != nil {
		return err
	}
	if p.opts.InstallTenant != nil {
		if err := p.opts.InstallTenant(ctx, key, cred); err != nil {
			return fmt.Errorf("clusterprovider: installing the tenant add-ons for %s after its "+
				"bootstrap: %w", ref.Name, err)
		}
	}
	p.opts.Log.Info("tenant bootstrap complete",
		"cluster", ref.Name, "key", key, "did", ref.DID,
		"grant", p.opts.GrantPath, "objects", objects,
		"credentialHost", cred.Host, "statusURL", cl.Status.URL,
	)
	return nil
}

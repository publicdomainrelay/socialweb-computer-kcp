package kcpclient

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func gateOf(cl *v1alpha1.Cluster) ClusterGate {
	return ClusterGate{Account: cl.Spec.Account,
		Serving: cl.Status.Phase == v1alpha1.ClusterReady && cl.DeletionTimestamp == nil}
}

func (c *Client) FollowClusterGate(ctx context.Context, home, name string, on func(ClusterGate)) error {
	r, err := c.resource(home, socialwebResource)
	if err != nil {
		return err
	}
	src := source[v1alpha1.Cluster]{r: r, resource: "clusters", field: "metadata.name=" + name, where: home}
	resync := func(items []v1alpha1.Cluster) error {
		g := ClusterGate{}
		for i := range items {
			if items[i].Name == name {
				g = gateOf(&items[i])
			}
		}
		on(g)
		return nil
	}
	items, rv, err := src.list(ctx)
	if err != nil {
		return err
	}
	if err := resync(items); err != nil {
		return err
	}
	return follow(ctx, src, rv, func(deleted bool, cl v1alpha1.Cluster) error {
		switch {
		case cl.Name != name:
		case deleted:
			on(ClusterGate{})
		default:
			on(gateOf(&cl))
		}
		return nil
	}, resync)
}

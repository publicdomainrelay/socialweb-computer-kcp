package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

type NodeVault struct {
	units   *UnitsHost
	cluster string
	dataDir string
	log     *slog.Logger
}

var _ nodemarket.NodeSecrets = (*NodeVault)(nil)

var _ nodemarket.NodeSecretsReconciler = (*NodeVault)(nil)

func NewNodeVault(units *UnitsHost, cluster, dataDir string) *NodeVault {
	return &NodeVault{units: units, cluster: cluster, dataDir: dataDir, log: slog.Default()}
}

func (v *NodeVault) WithLogger(log *slog.Logger) *NodeVault {
	if log != nil {
		v.log = log
	}
	return v
}

func (v *NodeVault) declare(ctx context.Context, change func(map[string]*v1alpha1.NodeClaimGrant)) error {
	if v.units == nil {
		return fmt.Errorf("%w (cluster %s): no units host", ErrNoClusterVault, v.cluster)
	}
	return v.units.UpdateVaultDeclared(ctx, v.cluster, func(d *v1alpha1.OpenBaoServerDeclared) {
		nodes := vaultNodes(d)
		change(nodes)
		setVaultNodes(d, nodes)
	})
}

func (v *NodeVault) EnsurePolicy(ctx context.Context, node string) error {
	if err := v.declare(ctx, func(nodes map[string]*v1alpha1.NodeClaimGrant) {
		if _, ok := nodes[node]; !ok {
			nodes[node] = nil
		}
	}); err != nil {
		return fmt.Errorf("clusterprovider: declaring policy %s in the cluster vault of %s: %w", vaultlayout.NodeJWTPolicyName(node), v.cluster, err)
	}
	return nil
}

func (v *NodeVault) WriteSecret(ctx context.Context, node string, secret nodemarket.NodeSecret) error {
	client, _, err := clusterVault(ctx, v.units.c.Logins, v.dataDir)
	if err != nil {
		return err
	}
	data := map[string]any{
		"token":            secret.Token,
		"nodePassword":     secret.NodePassword,
		"serverCA":         secret.ServerCA,
		"apiserverTicket":  secret.ApiserverTicket,
		"advertiseAddress": secret.AdvertiseAddress,
		"apiserverPort":    secret.ApiserverPort,
	}
	if err := client.PutKV(ctx, vaultlayout.NodeKVDataPath(node), data); err != nil {
		return fmt.Errorf("clusterprovider: writing %s: %w", vaultlayout.NodeKVDataPath(node), err)
	}
	return nil
}

func (v *NodeVault) EnsureLogin(ctx context.Context, node string, grant v1alpha1.NodeClaimGrant) (nodemarket.NodeLogin, error) {
	if !grantUsable(&grant) {
		return nodemarket.NodeLogin{}, fmt.Errorf("clusterprovider: a JWT role for node %s needs an "+
			"issuer, a subject and an audience, and the grant carries %q, %q, %q", node,
			grant.Issuer, grant.Subject, grant.Audience)
	}
	g := grant
	if err := v.declare(ctx, func(nodes map[string]*v1alpha1.NodeClaimGrant) { nodes[node] = &g }); err != nil {
		return nodemarket.NodeLogin{}, fmt.Errorf("clusterprovider: declaring the JWT login of node %s in the cluster vault of %s "+
			"(OpenBao fetches the discovery document of %s when the mount is configured, so the bidder's issuer must answer "+
			"from this host): %w", node, v.cluster, grant.Issuer, err)
	}
	return nodemarket.NodeLogin{Mount: vaultlayout.NodeJWTMount(grant.Issuer), Role: vaultlayout.NodeJWTRoleName(node),
		Policy: vaultlayout.NodeJWTPolicyName(node)}, nil
}

func (v *NodeVault) Revoke(ctx context.Context, node string) error {
	if err := v.declare(ctx, func(nodes map[string]*v1alpha1.NodeClaimGrant) { delete(nodes, node) }); err != nil {
		return fmt.Errorf("clusterprovider: removing node %s from the cluster vault of %s: %w", node, v.cluster, err)
	}
	v.log.Info("clusterprovider: node removed from the cluster vault's declaration; the sync deleted its login, policy and secret",
		"cluster", v.cluster, "node", node)
	return nil
}

func (v *NodeVault) DeclaredNodes(ctx context.Context) (map[string]bool, error) {
	if v.units == nil {
		return nil, fmt.Errorf("%w (cluster %s): no units host", ErrNoClusterVault, v.cluster)
	}
	cur, err := v.units.vaultServer(ctx, v.cluster)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%w (cluster %s)", ErrNoClusterVault, v.cluster)
	}
	out := map[string]bool{}
	for n, g := range vaultNodes(cur.Spec.Declared) {
		out[n] = grantUsable(g)
	}
	return out, nil
}

func (v *NodeVault) Reconcile(ctx context.Context, live map[string]*v1alpha1.NodeClaimGrant) error {
	err := v.declare(ctx, func(nodes map[string]*v1alpha1.NodeClaimGrant) {
		prev := map[string]*v1alpha1.NodeClaimGrant{}
		for n, g := range nodes {
			prev[n] = g
			delete(nodes, n)
		}
		for n, g := range live {
			switch {
			case grantUsable(g):
				c := *g
				nodes[n] = &c
			case grantUsable(prev[n]):
				nodes[n] = prev[n]
			default:
				nodes[n] = nil
			}
		}
	})
	if errors.Is(err, ErrNoClusterVault) && len(live) == 0 {
		return nil
	}
	return err
}

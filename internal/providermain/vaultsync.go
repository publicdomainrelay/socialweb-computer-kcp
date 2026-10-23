package providermain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
)

const VaultSyncInterval = 30 * time.Second

func (h *nodeHosts) reconcileVault(ctx context.Context, cluster string, claims []v1alpha1.NodeClaim) error {
	if h.opts.NodeSecrets == nil {
		return nil
	}
	now := h.opts.now()
	h.mu.Lock()
	if h.synced == nil {
		h.synced = map[string]time.Time{}
	}
	last, synced := h.synced[cluster]
	if synced && now.Sub(last) < VaultSyncInterval {
		h.mu.Unlock()
		return nil
	}
	h.synced[cluster] = now
	if h.vaultDue == nil {
		h.vaultDue = map[string]time.Time{}
	}
	delete(h.vaultDue, cluster)
	if len(claims) > 0 {
		h.vaultDue[cluster] = now.Add(VaultSyncInterval)
	}
	h.mu.Unlock()

	dir, err := clusterprovider.ClusterDataDir(h.opts.StateDir, cluster)
	if err != nil {
		return fmt.Errorf("clusterprovider: the data dir of %s, whose cluster OpenBao holds the roles, "+
			"policies and secrets of its market nodes: %w", cluster, err)
	}
	reconciler, ok := h.opts.NodeSecrets(cluster, dir).(nodemarket.NodeSecretsReconciler)
	if !ok {
		return nil
	}
	live := liveNodeGrants(cluster, claims, h.heldNames(cluster))
	if err := reconciler.Reconcile(ctx, live); err != nil {
		return fmt.Errorf("clusterprovider: making the cluster OpenBao of %s match the %d live market "+
			"node(s) it holds: %w -- a role, policy or secret with no live claim is one nothing revokes, "+
			"and it stays until a reconcile reads the vault again", cluster, len(live), err)
	}
	h.log.Info("cluster provider: the cluster OpenBao matches the live market nodes",
		"cluster", cluster, "live", len(live))
	return nil
}

func liveNodeGrants(cluster string, claims []v1alpha1.NodeClaim, held map[string]bool) map[string]*v1alpha1.NodeClaimGrant {
	prefix := strings.ToLower(cluster) + "-node"
	byName := map[string]v1alpha1.NodeClaim{}
	live := map[string]*v1alpha1.NodeClaimGrant{}
	for i := range claims {
		claim := claims[i]
		if claim.Spec.ClusterRef.Name != cluster {
			continue
		}
		if _, ok := nodeOrdinalInName(prefix, claim.Name); !ok {
			continue
		}
		byName[claim.Name] = claim
		if !nodemarket.Adoptable(claim) {
			continue
		}
		live[claim.Name] = claim.Status.Grant
	}
	for name := range held {
		if _, ok := live[name]; ok {
			continue
		}
		claim, ok := byName[name]
		if !ok {
			live[name] = nil
			continue
		}
		live[name] = claim.Status.Grant
	}
	return live
}

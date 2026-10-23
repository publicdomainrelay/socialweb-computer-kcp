package providermain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type claimAdopter interface {
	Adopt(claim v1alpha1.NodeClaim) error
}

func (h *nodeHosts) adopt(ctx context.Context, cluster string) error {
	if h.opts.Claims == nil {
		return nil
	}
	target, err := claimClusterFor(h.opts.Registry, cluster)(ctx)
	if err != nil {
		return fmt.Errorf("clusterprovider: the home workspace of %s, which holds the claims of the "+
			"market nodes a previous provider process rented: %w", cluster, err)
	}
	claims, err := h.opts.Claims.ListNodeClaims(ctx, target.Workspace)
	if err != nil {
		return fmt.Errorf("clusterprovider: listing the node claims in %s to adopt the live ones of "+
			"%s: %w -- a read that failed is not a workspace with no claims, and the next pass reads "+
			"it again", target.Workspace, cluster, err)
	}
	claims, orphanErr := h.claimJoinedOrphans(ctx, cluster, claims)
	if orphanErr != nil {
		return errors.Join(orphanErr, h.adoptClaims(ctx, cluster, claims))
	}
	return errors.Join(h.adoptClaims(ctx, cluster, claims), h.reconcileVault(ctx, cluster, claims))
}

func (h *nodeHosts) adoptOnce(ctx context.Context, cluster string) error {
	h.mu.Lock()
	done := h.adopted[cluster]
	h.mu.Unlock()
	if done {
		return nil
	}
	return h.adopt(ctx, cluster)
}

func (h *nodeHosts) adoptClaims(ctx context.Context, cluster string, claims []v1alpha1.NodeClaim) error {
	prefix := strings.ToLower(cluster) + "-node"
	var live []v1alpha1.NodeClaim
	for i := range claims {
		claim := claims[i]
		if claim.Spec.ClusterRef.Name != cluster || !nodemarket.Adoptable(claim) {
			continue
		}
		if _, ok := nodeOrdinalInName(prefix, claim.Name); !ok {
			continue
		}
		live = append(live, claim)
	}
	if len(live) == 0 {
		h.markAdopted(cluster)
		return nil
	}

	h.mu.Lock()
	n, ok := h.hosts[cluster]
	if !ok {
		built, err := newNodeHost(ctx, h.opts, h.log, cluster, nodeID)
		if err != nil {
			h.mu.Unlock()
			return err
		}
		built.host, built.nodeName = nil, ""
		h.hosts[cluster] = built
		n = built
	}
	h.mu.Unlock()

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started == 0 && n.host != nil {
		n.host, n.nodeName = nil, ""
	}
	held := map[string]bool{}
	for _, node := range n.heldNodesLocked() {
		held[node.name] = true
	}
	var problems []error
	adopted := 0
	for _, claim := range live {
		if held[claim.Name] {
			continue
		}
		ordinal, _ := nodeOrdinalInName(prefix, claim.Name)
		built, err := newNodeHost(ctx, h.opts, h.log, cluster, nodeIDFor(ordinal))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		adopter, ok := built.host.(claimAdopter)
		if !ok {
			problems = append(problems, fmt.Errorf("clusterprovider: the node host for %s cannot adopt a claim", claim.Name))
			continue
		}
		if err := adopter.Adopt(claim); err != nil {
			problems = append(problems, err)
			continue
		}
		n.retain(ordinal, heldNode{name: claim.Name, host: built.host})
		n.markLive(claim.Name)
		n.started++
		adopted++
		h.log.Info("cluster provider: adopted a live market node a previous provider process rented",
			"cluster", cluster, "node", claim.Name, "phase", claim.Status.Phase,
			"reason", "the guest joined with no session of ours and outlives this process, so its "+
				"claim is held again without being rewritten: the loss, scale-in and teardown rules "+
				"and the node count treat it like a node this process rented")
	}
	if adopted > 0 {
		if err := n.writeSnapshot(); err != nil {
			problems = append(problems, err)
		}
	}
	if len(problems) == 0 {
		h.markAdopted(cluster)
	}
	return errors.Join(problems...)
}

func (h *nodeHosts) releaseStranded(ctx context.Context, cluster string, claims []v1alpha1.NodeClaim) error {
	if h.opts.Claims == nil {
		return nil
	}
	prefix := strings.ToLower(cluster) + "-node"
	held := h.heldNames(cluster)
	var problems []error
	for i := range claims {
		claim := claims[i]
		if claim.Spec.ClusterRef.Name != cluster || claim.DeletionTimestamp != nil ||
			(claim.Status.Phase != v1alpha1.NodeClaimFailed && claim.Status.Phase != v1alpha1.NodeClaimLost) ||
			held[claim.Name] {
			continue
		}
		ordinal, ok := nodeOrdinalInName(prefix, claim.Name)
		if !ok {
			continue
		}
		built, err := newNodeHost(ctx, h.opts, h.log, cluster, nodeIDFor(ordinal))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		h.log.Info("cluster provider: deleting a Failed claim no provider process holds",
			"cluster", cluster, "node", claim.Name, "message", claim.Status.Message,
			"reason", "the door releases a claim's machine only when the claim is deleted, and a claim "+
				"that was already Failed when this process first read it is never adopted, so nothing "+
				"else would ever delete it and its machine would run on")
		if err := built.host.Reap(ctx); err != nil {
			problems = append(problems, fmt.Errorf("clusterprovider: releasing the Failed claim %s of %s: %w",
				claim.Name, cluster, err))
		}
	}
	return errors.Join(problems...)
}

type adoptionClaimWriter interface {
	WriteAdoptionClaim(ctx context.Context) error
}

func tenantNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (h *nodeHosts) claimJoinedOrphans(ctx context.Context, cluster string, claims []v1alpha1.NodeClaim) ([]v1alpha1.NodeClaim, error) {
	if h.opts.Claims == nil || h.opts.Tenant == nil || h.opts.NodeSecrets == nil {
		return claims, nil
	}
	dir, err := clusterprovider.ClusterDataDir(h.opts.StateDir, cluster)
	if err != nil {
		return claims, err
	}
	declared, ok := h.opts.NodeSecrets(cluster, dir).(nodemarket.NodeSecretsDeclared)
	if !ok {
		return claims, nil
	}
	prefix := strings.ToLower(cluster) + "-node"
	claimed := map[string]bool{}
	for i := range claims {
		claimed[claims[i].Name] = true
	}
	client, err := h.opts.Tenant(ctx, cluster)
	if err != nil {
		return claims, fmt.Errorf("clusterprovider: the tenant client of %s, to find joined Nodes with no claim: %w", cluster, err)
	}
	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return claims, fmt.Errorf("clusterprovider: listing the Nodes of %s to find joined Nodes with no claim: %w", cluster, err)
	}
	var vault map[string]bool
	var problems []error
	for i := range list.Items {
		node := &list.Items[i]
		ordinal, ok := nodeOrdinalInName(prefix, node.Name)
		if !ok || claimed[node.Name] || !tenantNodeReady(node) {
			continue
		}
		if vault == nil {
			if vault, err = declared.DeclaredNodes(ctx); err != nil {
				return claims, fmt.Errorf("clusterprovider: reading the cluster vault declaration of %s: %w", cluster, err)
			}
		}
		if _, held := vault[node.Name]; !held {
			h.log.Warn("cluster provider: a joined Node has no claim and the cluster vault holds no role for it, so it is not adopted",
				"cluster", cluster, "node", node.Name)
			continue
		}
		built, err := newNodeHost(ctx, h.opts, h.log, cluster, nodeIDFor(ordinal))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		writer, ok := built.host.(adoptionClaimWriter)
		if !ok {
			continue
		}
		if err := writer.WriteAdoptionClaim(ctx); err != nil {
			problems = append(problems, err)
			continue
		}
		claimed[node.Name] = true
		claims = append(claims, v1alpha1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			Spec: v1alpha1.NodeClaimSpec{ClusterRef: commonv1.LocalObjectRef{Name: cluster}, NodeName: node.Name, AdoptJoinedNode: true}})
		h.log.Info("cluster provider: a joined, Ready Node with no claim and a role in the cluster vault is claimed for adoption",
			"cluster", cluster, "node", node.Name,
			"next", "the door matches the claim to its receipt and vm.hostKeys record and moves it to Running, or fails it")
	}
	return claims, errors.Join(problems...)
}

func (h *nodeHosts) markAdopted(cluster string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.adopted == nil {
		h.adopted = map[string]bool{}
	}
	h.adopted[cluster] = true
}

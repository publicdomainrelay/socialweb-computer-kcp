package nodemarket

import (
	"errors"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var ErrNotAdoptable = errors.New(
	"nodemarket: only a live claim of this node is adopted -- an ended or deleting claim is the " +
		"door's record of a machine that is gone or going, and holding it would count a node " +
		"nothing is running")

func Adoptable(claim v1alpha1.NodeClaim) bool {
	return claim.DeletionTimestamp == nil && !EndedClaimPhase(claim.Status.Phase)
}

func JoinSecretSpent(phase v1alpha1.NodeClaimPhase) bool {
	return phase == v1alpha1.NodeClaimRunning
}

func (h *Host) Adopt(claim v1alpha1.NodeClaim) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.opts.Validate(); err != nil {
		return fmt.Errorf("nodemarket: node %s of %s: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	if claim.Name != h.opts.NodeName || claim.Spec.ClusterRef.Name != h.opts.Cluster || !Adoptable(claim) {
		return fmt.Errorf("%w (claim %s of cluster %q, phase %q, deleting %v; this host is node %s of %s)",
			ErrNotAdoptable, claim.Name, claim.Spec.ClusterRef.Name, claim.Status.Phase,
			claim.DeletionTimestamp != nil, h.opts.NodeName, h.opts.Cluster)
	}
	h.provisioned = true
	h.policyWritten = true
	h.secretWritten = JoinSecretSpent(claim.Status.Phase)
	h.heldBack = ""
	h.granted = nil
	h.log().Info("nodemarket: adopted a live claim a previous provider process wrote",
		"cluster", h.opts.Cluster, "node", h.opts.NodeName, "phase", claim.Status.Phase,
		"secretRewritten", !h.secretWritten,
		"next", "the claim is not written again; the confirm pass reads it, ensures the JWT role "+
			"from status.grant, and the loss, scale-in and teardown rules hold it like any other")
	return nil
}

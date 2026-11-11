package atproto

import (
	"context"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const AssociationRecheck = time.Minute

func (a *Accounts) Associate(ctx context.Context, id *v1alpha1.MarketIdentity, watch *v1alpha1.AtprotoRecordWatch, status *v1alpha1.MarketIdentityStatus) {
	a.refreshWalk(id, watch, status)
	declared := uniqueDIDs(id.Spec.ApprovedAssociations)
	previous := make([]string, 0, len(status.Associations))
	for _, did := range uniqueDIDs(status.Associations) {
		if !slices.Contains(declared, did) {
			previous = append(previous, did)
		}
	}
	verified, observed := a.verifiedPrincipals(ctx, id, watch, status)
	if !observed {
		verified = previous
	}
	removed := false
	for _, did := range previous {
		if !slices.Contains(verified, did) {
			removed = true
		}
	}
	status.Associations = uniqueDIDs(append(slices.Clone(declared), verified...))
	if removed && len(status.Associations) == 0 {
		conds.Set(&status.Conditions, id.Generation, v1alpha1.ConditionAssociated, false, ReasonRevoked, "the identlink walk of the identity reaches no principal any more")
		return
	}
	setAssociated(status, id.Generation, id.Spec.Association)
}

func (a *Accounts) verifiedPrincipals(ctx context.Context, id *v1alpha1.MarketIdentity, watch *v1alpha1.AtprotoRecordWatch, status *v1alpha1.MarketIdentityStatus) ([]string, bool) {
	if watch == nil || id.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo || id.Spec.Association == v1alpha1.AssociationSkip {
		return nil, false
	}
	subject := strings.TrimSpace(status.Did)
	if subject == "" {
		return nil, false
	}
	if !meta.IsStatusConditionTrue(watch.Status.Conditions, v1alpha1.ConditionVerified) && len(watch.Status.Deleted) == 0 {
		return nil, false
	}
	walked, err := a.trust.Walk(ctx, subject, trust.Association{})
	if err != nil {
		return nil, false
	}
	var out []string
	for _, root := range walked.Roots {
		if operator := strings.TrimSpace(id.Spec.AssociateWith); root == subject || strings.HasPrefix(operator, "did:") && root != operator {
			continue
		}
		out = append(out, root)
	}
	slices.Sort(out)
	return slices.Compact(out), true
}

func (a *Accounts) refreshWalk(id *v1alpha1.MarketIdentity, watch *v1alpha1.AtprotoRecordWatch, status *v1alpha1.MarketIdentityStatus) {
	subject := strings.TrimSpace(status.Did)
	if subject == "" || id.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo || id.Spec.Association == v1alpha1.AssociationSkip {
		return
	}
	var seen strings.Builder
	if watch != nil {
		for _, r := range watch.Status.Records {
			seen.WriteString(r.URI + "#" + r.Cid + " ")
		}
		seen.WriteString(strings.Join(watch.Status.Deleted, " "))
	}
	if last, ok := a.watched.Get(subject); ok && last == seen.String() && associatedTrue(status) {
		return
	}
	a.watched.Set(subject, seen.String())
	a.trust.ApplyEvent(subject, "")
}

func uniqueDIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, did := range in {
		if did = strings.TrimSpace(did); did != "" && !slices.Contains(out, did) {
			out = append(out, did)
		}
	}
	return out
}

func associatedTrue(status *v1alpha1.MarketIdentityStatus) bool {
	return meta.IsStatusConditionTrue(status.Conditions, v1alpha1.ConditionAssociated)
}

func setAssociated(status *v1alpha1.MarketIdentityStatus, generation int64, mode v1alpha1.AssociationMode) {
	if refCondition(status, generation) {
		return
	}
	if mode == v1alpha1.AssociationSkip {
		conds.Set(&status.Conditions, generation, v1alpha1.ConditionAssociated, true, "AssociationSkipped", "the association is skipped")
		return
	}
	if len(status.Associations) > 0 {
		conds.Set(&status.Conditions, generation, v1alpha1.ConditionAssociated, true, "Associated", "the operator association is approved")
		return
	}
	if revoked := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionAssociated); revoked != nil && revoked.Reason == ReasonRevoked {
		conds.Set(&status.Conditions, generation, v1alpha1.ConditionAssociated, false, ReasonRevoked, revoked.Message)
		return
	}
	conds.Set(&status.Conditions, generation, v1alpha1.ConditionAssociated, false, "AwaitingAssociation", "the PDS is ready; the association is pending")
}

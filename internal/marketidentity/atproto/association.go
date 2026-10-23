package atproto

import (
	"encoding/json"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

func Associate(id *v1alpha1.MarketIdentity, watch *v1alpha1.AtprotoRecordWatch, status *v1alpha1.MarketIdentityStatus) {
	declared := uniqueDIDs(id.Spec.ApprovedAssociations)
	previous := make([]string, 0, len(status.Associations))
	for _, did := range uniqueDIDs(status.Associations) {
		if !slices.Contains(declared, did) {
			previous = append(previous, did)
		}
	}
	verified, observed := verifiedClaims(id, watch, status)
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
		conds.Set(&status.Conditions, id.Generation, v1alpha1.ConditionAssociated, false, ReasonRevoked, "the operator's verified claim record is gone")
		return
	}
	setAssociated(status, id.Generation, id.Spec.Association)
}

func verifiedClaims(id *v1alpha1.MarketIdentity, watch *v1alpha1.AtprotoRecordWatch, status *v1alpha1.MarketIdentityStatus) ([]string, bool) {
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
	var out []string
	for _, record := range watch.Status.Records {
		did := associationClaimDid(record, subject, id.Spec.AssociationService)
		if did == "" {
			continue
		}
		if operator := strings.TrimSpace(id.Spec.AssociateWith); strings.HasPrefix(operator, "did:") && did != operator {
			continue
		}
		out = append(out, did)
	}
	slices.Sort(out)
	return slices.Compact(out), true
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

func associationClaimDid(record v1alpha1.WatchedRecord, subject, service string) string {
	var value struct {
		Challenge string `json:"challenge"`
		KeyID     string `json:"keyId"`
		Service   string `json:"service"`
	}
	if err := json.Unmarshal(record.Value, &value); err != nil {
		return ""
	}
	challenge := strings.TrimSpace(value.Challenge)
	keyID := strings.TrimSpace(value.KeyID)
	if strings.TrimSpace(service) == "" || strings.TrimSpace(value.Service) != service {
		return ""
	}
	if strings.HasPrefix(challenge, "did:") && keyID == subject && record.Did == challenge {
		return challenge
	}
	return ""
}

func associatedTrue(status *v1alpha1.MarketIdentityStatus) bool {
	return meta.IsStatusConditionTrue(status.Conditions, v1alpha1.ConditionAssociated)
}

func setAssociated(status *v1alpha1.MarketIdentityStatus, generation int64, mode v1alpha1.AssociationMode) {
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

package atproto

import (
	"encoding/json"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

type Association struct {
	Approved []string

	Changed bool
}

func Associate(id *v1alpha1.MarketIdentity, watch *v1alpha1.AtprotoRecordWatch, status *v1alpha1.MarketIdentityStatus) Association {
	current := Association{Approved: id.Spec.ApprovedAssociations}
	if watch == nil || id.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo || id.Spec.Association == v1alpha1.AssociationSkip {
		return current
	}
	subject := strings.TrimSpace(status.Did)
	if subject == "" {
		return current
	}
	if !meta.IsStatusConditionTrue(watch.Status.Conditions, v1alpha1.ConditionVerified) && len(watch.Status.Deleted) == 0 {
		return current
	}
	approved := map[string]bool{}
	for _, record := range watch.Status.Records {
		did := associationClaimDid(record, subject, id.Spec.AssociationService)
		if did == "" {
			continue
		}
		if operator := strings.TrimSpace(id.Spec.AssociateWith); strings.HasPrefix(operator, "did:") && did != operator {
			continue
		}
		approved[did] = true
	}
	next := make([]string, 0, len(id.Spec.ApprovedAssociations)+len(approved))
	for _, did := range id.Spec.ApprovedAssociations {
		if approved[did] {
			next = append(next, did)
			delete(approved, did)
		}
	}
	fresh := make([]string, 0, len(approved))
	for did := range approved {
		fresh = append(fresh, did)
	}
	slices.Sort(fresh)
	next = append(next, fresh...)
	removed := len(next) < len(id.Spec.ApprovedAssociations)
	status.Associations = append([]string(nil), next...)
	generation := id.Generation
	if removed {
		conds.Set(&status.Conditions, generation, v1alpha1.ConditionAssociated, false, ReasonRevoked, "the operator's verified claim record is gone")
	} else {
		spec := id.Spec
		spec.ApprovedAssociations = next
		setAssociated(status, generation, spec)
	}
	return Association{Approved: next, Changed: !slices.Equal(next, id.Spec.ApprovedAssociations)}
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

func setAssociated(status *v1alpha1.MarketIdentityStatus, generation int64, spec v1alpha1.MarketIdentitySpec) {
	associations := make([]string, 0, len(spec.ApprovedAssociations))
	for _, did := range spec.ApprovedAssociations {
		did = strings.TrimSpace(did)
		if did == "" || slices.Contains(associations, did) {
			continue
		}
		associations = append(associations, did)
	}
	status.Associations = associations
	if spec.Association == v1alpha1.AssociationSkip {
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

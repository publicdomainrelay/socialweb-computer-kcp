package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

const (
	AssociateConfirmNSID = "com.publicdomainrelay.temp.requester.associateConfirm"
	BadgeBlueKeysNSID    = "com.publicdomainrelay.temp.badgeBlueKeys"
	BidderAssociate      = "bidder_associate"
	AssociationWatch     = "association"
)

func (ctl *Controller) associateNetworkOperator(ctx context.Context, spec v1alpha1.MarketBidderSpec) error {
	id, err := ctl.bidderIdentity(ctx)
	if err != nil || !claimOpen(id) || spec.Identity.Handle == "" || spec.Network == nil {
		return err
	}
	password, _, err := ctl.bidderPassword(ctx)
	if err != nil {
		return err
	}
	operator, err := ctl.writeClaim(ctx, id, spec.Identity.PdsURL, spec.Network.CABundle, spec.Identity.Handle, password, "bidder-", BidderAssociate)
	if err != nil || operator == "" {
		return err
	}
	ctl.c.log().Info("the market network operator account wrote its claim record naming the bidder's own DID", "operator", operator,
		"handle", spec.Identity.Handle, "bidderDid", id.Status.Did)
	return nil
}

func claimOpen(id *v1alpha1.MarketIdentity) bool {
	if id == nil || strings.TrimSpace(id.Status.Did) == "" {
		return false
	}
	for _, c := range id.Status.Conditions {
		if c.Type == v1alpha1.ConditionAssociated && c.Reason == "Revoked" {
			return false
		}
	}
	if id.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		if id.Spec.Association == v1alpha1.AssociationSkip || associatedClaim(id) || len(id.Status.Associations) > 0 {
			return false
		}
		if id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingApproval {
			return true
		}
		return id.Status.Phase == v1alpha1.MarketIdentityPhaseReady && len(id.Spec.ApprovedAssociations) == 0
	}
	return id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingScan && id.Status.PendingAction != nil
}

func claimOf(rawURL string) (nonce, service string) {
	_, frag, _ := strings.Cut(rawURL, "#")
	q, _ := url.ParseQuery(frag)
	return q.Get("n"), q.Get("s")
}

func (ctl *Controller) writeClaim(ctx context.Context, id *v1alpha1.MarketIdentity, operatorPds, ca, identifier, password, namePrefix, service string) (string, error) {
	if id == nil || strings.TrimSpace(service) == "" {
		return "", nil
	}
	nonce, pendingService := "", ""
	if action := id.Status.PendingAction; action != nil {
		nonce, pendingService = claimOf(action.URL)
	}
	if id.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo && nonce == "" {
		return "", nil
	}
	if pendingService != "" && pendingService != service {
		return "", fmt.Errorf("the pending association names service %q, not %q", pendingService, service)
	}
	didText := strings.TrimSpace(id.Status.Did)
	if _, err := syntax.ParseDID(didText); err != nil {
		return "", fmt.Errorf("the identity's did %q: %w", didText, err)
	}
	pds, err := ctl.pdsFor(operatorPds, ca)
	if err != nil {
		return "", err
	}
	sess, err := pds.CreateSession(ctx, identifier, password)
	if err != nil {
		return "", err
	}
	operator := sess.DID
	rkey := syntax.RecordKey(service + "-" + strings.ReplaceAll(didText, ":", "-"))
	path := pdsclient.RepoPath{Repo: operator, Collection: syntax.NSID(BadgeBlueKeysNSID), Rkey: rkey}
	if held, err := pds.GetRecord(ctx, path, nil); err == nil {
		var value map[string]any
		if json.Unmarshal(held.Value, &value) == nil && heldClaimMatches(value, nonce, operator.String(), didText, service) {
			return "", nil
		}
	}
	fields := map[string]any{"$type": BadgeBlueKeysNSID, "keyId": didText, "challenge": operator.String(), "service": service,
		"name": namePrefix + didText[len(didText)-8:], "createdAt": time.Now().UTC().Format(time.RFC3339)}
	if nonce != "" {
		fields["nonce"] = nonce
	}
	record, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	if _, err := pds.PutRecord(ctx, path, record, pdsclient.WriteOptions{}); err != nil {
		return "", err
	}
	return operator.String(), nil
}

func heldClaimMatches(value map[string]any, nonce, operator, subject, service string) bool {
	if value == nil {
		return false
	}
	challenge, _ := value["challenge"].(string)
	keyID, _ := value["keyId"].(string)
	if challenge != operator || keyID != subject {
		return false
	}
	if got, _ := value["service"].(string); got != service {
		return false
	}
	if got, _ := value["nonce"].(string); got != "" && got != nonce {
		return false
	}
	return true
}

func associationRequired(plan accountPlan, id *v1alpha1.MarketIdentity) bool {
	if plan.Mode != v1alpha1.MarketIdentityModeLocalRepo || plan.Identity.Association == v1alpha1.AssociationSkip || id == nil {
		return false
	}
	return strings.TrimSpace(id.Spec.AssociationWatch) != "" || strings.TrimSpace(id.Spec.AssociateWith) != ""
}

func associationSatisfied(plan accountPlan, id *v1alpha1.MarketIdentity) bool {
	if !associationRequired(plan, id) {
		return true
	}
	if id == nil || id.Status.Phase != v1alpha1.MarketIdentityPhaseReady {
		return false
	}
	return associatedClaim(id) || len(id.Status.Associations) > 0
}

func associationWait(plan accountPlan, id *v1alpha1.MarketIdentity, service string) string {
	if !associationRequired(plan, id) || associationSatisfied(plan, id) {
		return ""
	}
	keyID := "the requester DID"
	if id != nil {
		if did := strings.TrimSpace(id.Status.Did); did != "" {
			keyID = did
		}
	}
	return fmt.Sprintf("waiting for the operator's %s claim record (challenge=operator, keyId=%s, service=%s)", service, keyID, service)
}

func associatedClaim(id *v1alpha1.MarketIdentity) bool {
	if id == nil {
		return false
	}
	for _, c := range id.Status.Conditions {
		if c.Type == v1alpha1.ConditionAssociated && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func (ctl *Controller) identityIn(ctx context.Context, ws, name string) (*v1alpha1.MarketIdentity, error) {
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var id v1alpha1.MarketIdentity
	if err := unstruct.Into(u, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

func (ctl *Controller) associateGatewayOperator(ctx context.Context, id *v1alpha1.MarketIdentity, sessionsFile string, n *v1alpha1.MarketNetworkValue) error {
	if !claimOpen(id) || n == nil {
		return nil
	}
	s, err := firstOwnerSession(sessionsFile)
	if err != nil {
		return err
	}
	operator, err := ctl.writeClaim(ctx, id, s.Service, n.CABundle, s.Identifier, s.Password, "requester-", RequesterAssociate)
	if err != nil || operator == "" {
		return err
	}
	ctl.c.log().Info("the market network operator account wrote its claim record naming the gateway requester's own DID", "operator", operator,
		"handle", s.Identifier, "requesterDid", id.Status.Did)
	return nil
}

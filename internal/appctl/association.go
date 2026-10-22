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
)

const (
	AssociateConfirmNSID = "com.publicdomainrelay.temp.requester.associateConfirm"
	BadgeBlueKeysNSID    = "com.publicdomainrelay.temp.badgeBlueKeys"
	BidderAssociate      = "bidder_associate"
	AssociationWatch     = "association"
)

func (ctl *Controller) associateNetworkOperator(ctx context.Context, spec v1alpha1.MarketBidderSpec) error {
	id, err := ctl.bidderIdentity(ctx)
	if err != nil || !claimOpen(id) || spec.Identity.Handle == "" {
		return err
	}
	password, _, err := ctl.bidderPassword(ctx)
	if err != nil {
		return err
	}
	operator, err := ctl.writeClaim(ctx, id, spec.Identity.PdsUrl, spec.Network.CaBundleRef, spec.Identity.Handle, password, "bidder-")
	if err != nil || operator == "" {
		return err
	}
	ctl.c.log().Info("the market network operator account wrote its claim record naming the bidder's own DID", "operator", operator,
		"handle", spec.Identity.Handle, "bidderDid", id.Status.Did)
	return nil
}

func claimOpen(id *v1alpha1.MarketIdentity) bool {
	if id == nil || id.Status.Phase != v1alpha1.MarketIdentityPhaseAwaitingScan || id.Status.Did == "" || id.Status.PendingAction == nil {
		return false
	}
	for _, c := range id.Status.Conditions {
		if c.Type == v1alpha1.ConditionAssociated && c.Reason == "Revoked" {
			return false
		}
	}
	return true
}

func claimOf(rawURL string) (nonce, service string) {
	_, frag, _ := strings.Cut(rawURL, "#")
	q, _ := url.ParseQuery(frag)
	return q.Get("n"), q.Get("s")
}

func (ctl *Controller) writeClaim(ctx context.Context, id *v1alpha1.MarketIdentity, operatorPds, ca, identifier, password, namePrefix string) (string, error) {
	nonce, service := claimOf(id.Status.PendingAction.Url)
	if nonce == "" || service == "" {
		return "", nil
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
		var value struct {
			Nonce string `json:"nonce"`
		}
		if json.Unmarshal(held.Value, &value) == nil && value.Nonce == nonce {
			return "", nil
		}
	}
	record, err := json.Marshal(map[string]any{"$type": BadgeBlueKeysNSID, "keyId": didText, "challenge": operator.String(), "service": service,
		"nonce": nonce, "name": namePrefix + didText[len(didText)-8:], "createdAt": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return "", err
	}
	if _, err := pds.PutRecord(ctx, path, record, pdsclient.WriteOptions{}); err != nil {
		return "", err
	}
	return operator.String(), nil
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
	if err := fromUnstructured(u, &id); err != nil {
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
	operator, err := ctl.writeClaim(ctx, id, s.Service, n.CaBundleRef, s.Identifier, s.Password, "requester-")
	if err != nil || operator == "" {
		return err
	}
	ctl.c.log().Info("the market network operator account wrote its claim record naming the gateway requester's own DID", "operator", operator,
		"handle", s.Identifier, "requesterDid", id.Status.Did)
	return nil
}

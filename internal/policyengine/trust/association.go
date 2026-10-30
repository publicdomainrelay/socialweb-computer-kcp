package trust

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
)

const (
	KindClaim       = "claim"
	KindAssociation = "association"
	KindBskyPost    = "bskyPost"
	KindDelvePost   = "delvePost"

	DefaultPostTemplate = "associate {did}"
)

var DefaultKinds = []string{KindClaim, KindAssociation}

type Association struct {
	Kinds           []string
	PostTemplate    string
	MaxDepth        int
	RequireContract bool
}

type associationKey struct {
	claim           bool
	association     bool
	bskyPost        bool
	delvePost       bool
	template        string
	requireContract bool
}

func (a Association) key() associationKey {
	kinds := a.Kinds
	if len(kinds) == 0 {
		kinds = DefaultKinds
	}
	template := a.PostTemplate
	if strings.TrimSpace(template) == "" {
		template = DefaultPostTemplate
	}
	return associationKey{
		claim:           slices.Contains(kinds, KindClaim),
		association:     slices.Contains(kinds, KindAssociation),
		bskyPost:        slices.Contains(kinds, KindBskyPost),
		delvePost:       slices.Contains(kinds, KindDelvePost),
		template:        template,
		requireContract: a.RequireContract,
	}
}

func (k associationKey) allows(kind string) bool {
	switch kind {
	case KindClaim:
		return k.claim
	case KindAssociation:
		return k.association
	case KindBskyPost:
		return k.bskyPost
	case KindDelvePost:
		return k.delvePost
	}
	return false
}

func KindOfCollection(collection string) string {
	switch collection {
	case BadgeBlueKeysNSID:
		return KindClaim
	case AssociationNSID:
		return KindAssociation
	case BskyPostNSID:
		return KindBskyPost
	case DelvePostNSID:
		return KindDelvePost
	}
	return ""
}

func collectionOfKind(kind string) string {
	switch kind {
	case KindClaim:
		return BadgeBlueKeysNSID
	case KindAssociation:
		return AssociationNSID
	case KindBskyPost:
		return BskyPostNSID
	case KindDelvePost:
		return DelvePostNSID
	}
	return ""
}

func Matches(kind string, value map[string]any, authority, subject, service, template string) bool {
	if value == nil || authority == "" || subject == "" || authority == subject {
		return false
	}
	if recordType, _ := value["$type"].(string); recordType != collectionOfKind(kind) {
		return false
	}
	switch kind {
	case KindClaim:
		held, ok := claimOf(value)
		return ok && held.operator == authority && held.subject == subject && associationService(held.service, service)
	case KindAssociation:
		recordSubject, _ := value["subject"].(string)
		recordService, _ := value["service"].(string)
		return recordSubject == subject && associationService(recordService, service)
	case KindBskyPost, KindDelvePost:
		return postMatches(value, subject, template)
	}
	return false
}

func postMatches(value map[string]any, subject, template string) bool {
	if strings.TrimSpace(template) == "" {
		template = DefaultPostTemplate
	}
	text, _ := value["text"].(string)
	if strings.TrimSpace(text) != strings.TrimSpace(strings.ReplaceAll(template, "{did}", subject)) {
		return false
	}
	mentions := 0
	matched := false
	facets, _ := value["facets"].([]any)
	for _, facet := range facets {
		entry, _ := facet.(map[string]any)
		features, _ := entry["features"].([]any)
		for _, feature := range features {
			mention, _ := feature.(map[string]any)
			if featureType, _ := mention["$type"].(string); featureType != MentionFacetType {
				continue
			}
			mentions++
			did, _ := mention["did"].(string)
			matched = did == subject
		}
	}
	return mentions == 1 && matched
}

func strongRefOf(value any) (*atproto.StrongRef, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	uri, _ := fields["uri"].(string)
	cid, _ := fields["cid"].(string)
	if !strings.HasPrefix(uri, "at://") || cid == "" {
		return nil, false
	}
	return &atproto.StrongRef{URI: uri, CID: cid}, true
}

func (r *Resolver) pinnedRecord(ctx context.Context, ref atproto.StrongRef) (map[string]any, string, error) {
	record, err := r.client.GetRecordAt(ctx, ref)
	if errors.Is(err, atproto.ErrRecordNotFound) {
		return nil, "the record no longer resolves", nil
	}
	if err != nil {
		return nil, "", err
	}
	if record.CID != ref.CID {
		return nil, "the record cid changed", nil
	}
	return record.Value, "", nil
}

func (r *Resolver) deny(reason string, want claim, meta map[string]any) (bool, error) {
	fields := map[string]any{"did": want.subject, "operator": want.operator, "service": want.service, "reason": reason}
	for key, value := range meta {
		fields[key] = value
	}
	r.log("info", "association denied", fields)
	return false, nil
}

func (r *Resolver) proofHeld(ctx context.Context, want claim, proof atproto.StrongRef, assoc associationKey) (bool, error) {
	repo, collection, _ := atproto.SplitURI(proof.URI)
	kind := KindOfCollection(collection)
	meta := map[string]any{"proof": proof.URI, "cid": proof.CID}
	switch {
	case repo != want.operator:
		return r.deny("the proof is not in the authority repo", want, meta)
	case kind == "" || !assoc.allows(kind):
		return r.deny("the proof kind is not accepted", want, meta)
	}
	value, reason, err := r.pinnedRecord(ctx, proof)
	if err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return r.deny("proof: "+reason, want, meta)
	}
	if !Matches(kind, value, want.operator, want.subject, want.service, assoc.template) {
		return r.deny("the proof does not match", want, meta)
	}
	if kind == KindBskyPost || kind == KindDelvePost {
		if assoc.requireContract {
			return r.deny("a post carries no contract", want, meta)
		}
		return true, nil
	}
	return r.scopeHeld(ctx, value, want, assoc)
}

func (r *Resolver) associationHeld(ctx context.Context, want claim, assoc associationKey) (bool, error) {
	value, err := r.getRecord(ctx, want.operator, AssociationNSID, ClaimRKey(want.service, want.subject))
	if err != nil || value == nil {
		return false, err
	}
	if !Matches(KindAssociation, value, want.operator, want.subject, want.service, assoc.template) {
		return r.deny("the association record does not match", want, nil)
	}
	return r.scopeHeld(ctx, value, want, assoc)
}

func (r *Resolver) scopeHeld(ctx context.Context, value map[string]any, want claim, assoc associationKey) (bool, error) {
	rawContract, scoped := value["contract"]
	rawExpires, expiring := value["expiresAt"]
	expiresAt, _ := rawExpires.(string)
	switch {
	case scoped && expiresAt == "":
		return r.deny("a contract-scoped association has no expiresAt", want, nil)
	case assoc.requireContract && !scoped:
		return r.deny("the verifier requires a contract-scoped association", want, nil)
	}
	if expiring {
		deadline, err := time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil {
			return r.deny("the expiresAt is not a datetime", want, map[string]any{"expiresAt": expiresAt})
		}
		if !r.now().Before(deadline) {
			return r.deny("the association expired", want, map[string]any{"expiresAt": expiresAt})
		}
	}
	if !scoped {
		return true, nil
	}
	contract, ok := strongRefOf(rawContract)
	if !ok {
		return r.deny("the contract is not a strongRef", want, nil)
	}
	return r.contractHeld(ctx, *contract, want)
}

func (r *Resolver) contractHeld(ctx context.Context, contract atproto.StrongRef, want claim) (bool, error) {
	meta := map[string]any{"contract": contract.URI, "cid": contract.CID}
	repo, collection, _ := atproto.SplitURI(contract.URI)
	if repo != want.operator || collection != market.AcceptNSID {
		return r.deny("the contract is not a market.accept of the authority", want, meta)
	}
	accept, reason, err := r.pinnedRecord(ctx, contract)
	if err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return r.deny("contract: "+reason, want, meta)
	}
	bid, ok := strongRefOf(accept["bid"])
	if !ok {
		return r.deny("the contract names no bid", want, meta)
	}
	bidder := atproto.RepoFromURI(bid.URI)
	receiptRKey := market.ReceiptRKey(contract.URI, contract.CID)
	receipt, err := r.getRecord(ctx, bidder, market.ReceiptNSID, receiptRKey)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		return r.deny("the contract has no receipt", want, meta)
	}
	if answered, ok := strongRefOf(receipt["accept"]); !ok || *answered != contract {
		return r.deny("the receipt does not answer the contract", want, meta)
	}
	receiptURI := "at://" + bidder + "/" + market.ReceiptNSID + "/" + receiptRKey
	released, err := r.getRecord(ctx, want.operator, market.ComputeEventsVMDeleteNSID, market.ReleasePayloadRKey(receiptURI))
	if err != nil {
		return false, err
	}
	if released != nil {
		return r.deny("the contract was released", want, meta)
	}
	terminationRKey, _ := market.EventRKeys(market.TerminationKey(receiptURI))
	terminated, err := r.getRecord(ctx, bidder, market.ComputeEventsVMTerminatedNSID, terminationRKey)
	if err != nil {
		return false, err
	}
	if terminated != nil {
		return r.deny("the contract was terminated", want, meta)
	}
	return true, nil
}

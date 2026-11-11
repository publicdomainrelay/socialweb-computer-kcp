package trust

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
)

const MaxChainDepth = identlink.DefaultMaxDepth

type Association struct {
	MaxDepth        int
	RequireContract bool
}

func (a Association) normal() Association {
	if a.MaxDepth <= 0 || a.MaxDepth > MaxChainDepth {
		a.MaxDepth = MaxChainDepth
	}
	return a
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

func (r *Resolver) deny(reason string, contract atproto.StrongRef, meta map[string]any) (bool, error) {
	fields := map[string]any{"contract": contract.URI, "cid": contract.CID, "reason": reason}
	for key, value := range meta {
		fields[key] = value
	}
	r.log("info", "contract not held", fields)
	return false, nil
}

func (r *Resolver) ContractHeld(ctx context.Context, contract atproto.StrongRef, authorities []string) (bool, error) {
	return r.contractHeld(ctx, contract, authorities)
}

func (r *Resolver) contractHeld(ctx context.Context, contract atproto.StrongRef, authorities []string) (bool, error) {
	repo, collection, _ := atproto.SplitURI(contract.URI)
	if !slices.Contains(authorities, repo) || collection != market.AcceptNSID {
		return r.deny("the contract is not a market.accept of the principal or of a principal above it", contract, nil)
	}
	accept, reason, err := r.pinnedRecord(ctx, contract)
	if err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return r.deny("contract: "+reason, contract, nil)
	}
	bid, ok := strongRefOf(accept["bid"])
	if !ok {
		return r.deny("the contract names no bid", contract, nil)
	}
	bidder := atproto.RepoFromURI(bid.URI)
	if !strings.HasPrefix(bidder, "did:") {
		return r.deny("the contract's bid is not in a DID repo", contract, nil)
	}
	receiptURI := "at://" + bidder + "/" + market.ReceiptNSID + "/" + market.ReceiptRKey(contract.URI, contract.CID)
	receipt, err := r.client.GetRecordAt(ctx, atproto.StrongRef{URI: receiptURI})
	if errors.Is(err, atproto.ErrRecordNotFound) {
		return r.deny("the contract has no receipt", contract, nil)
	}
	if err != nil {
		return false, err
	}
	if receipt.URI != receiptURI {
		return r.deny("the receipt uri is not canonical", contract, map[string]any{"receipt": receipt.URI})
	}
	if answered, ok := strongRefOf(receipt.Value["accept"]); !ok || *answered != contract {
		return r.deny("the receipt does not answer the contract", contract, nil)
	}
	released, err := r.getRecord(ctx, repo, market.ComputeEventsVMDeleteNSID, market.ReleasePayloadRKey(receiptURI))
	if err != nil {
		return false, err
	}
	if released != nil {
		return r.deny("the contract was released", contract, nil)
	}
	terminationRKey, _ := market.EventRKeys(market.TerminationKey(receiptURI))
	terminated, err := r.getRecord(ctx, bidder, market.ComputeEventsVMTerminatedNSID, terminationRKey)
	if err != nil {
		return false, err
	}
	if terminated != nil {
		return r.deny("the contract was terminated", contract, nil)
	}
	return true, nil
}

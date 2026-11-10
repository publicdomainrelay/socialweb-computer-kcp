package bidder

import (
	"context"
	"fmt"
	"slices"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

var ErrKindUnsupported = fmt.Errorf("bidder: the RFP asks for a compute kind this bidder does not serve")

func (b *Bidder) servesKind(kind string) bool {
	served := b.cfg.ComputeKinds
	if len(served) == 0 {
		served = []string{string(v1alpha1.ComputeKindVM)}
	}
	if kind == "" {
		kind = string(v1alpha1.ComputeKindVM)
	}
	if !slices.Contains(served, kind) {
		return false
	}
	if kind == string(v1alpha1.ComputeKindDeno) && (b.cfg.Sandbox == nil || !b.cfg.Sandbox.Available()) {
		return false
	}
	return true
}

func (b *Bidder) rfpKind(ctx context.Context, rfpURI, rfpCID string) (string, error) {
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: rfpURI, CID: rfpCID})
	if err != nil {
		return "", fmt.Errorf("bidder: resolving the RFP payload kind: %w", err)
	}
	rfp, err := market.ParseRFP(resolved.Value)
	if err != nil {
		return "", nil
	}
	payload, err := b.records.Resolve(ctx, market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return "", fmt.Errorf("bidder: resolving the RFP payload: %w", err)
	}
	kind, _ := payload.Value["kind"].(string)
	return kind, nil
}

func (b *Bidder) inboxServesKind(ctx context.Context, rfpURI, rfpCID string) (string, bool) {
	kind, err := b.rfpKind(ctx, rfpURI, rfpCID)
	if err != nil {
		b.log.Warn("reading the compute kind of an inbox RFP failed; it is dropped", "rfp", rfpURI, "err", err.Error())
		return kind, false
	}
	return kind, b.servesKind(kind)
}

func (b *Bidder) payloadRefused(ctx context.Context, rfp *market.RFP) (string, error) {
	payload, err := b.records.Resolve(ctx, market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return "", fmt.Errorf("bidder: resolving the RFP payload: %w", err)
	}
	if _, err := parseVMRecord(payload.Value); err != nil {
		return err.Error(), nil
	}
	return "", nil
}

func (b *Bidder) guestToolsRefused(ctx context.Context, rfp *market.RFP) (string, error) {
	payload, err := b.records.Resolve(ctx, market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return "", fmt.Errorf("bidder: resolving the RFP payload for its guest tools: %w", err)
	}
	userData, _ := payload.Value["user_data"].(string)
	needs := guestseed.GuestBinariesNeeded(userData)
	if len(needs.Tools) == 0 {
		return "", nil
	}
	if err := needs.ToolsErr(); err != nil {
		return err.Error(), nil
	}
	if _, err := guestseed.GuestBinaryPins(b.cfg.GuestDownloads, needs.Tools); err != nil {
		return err.Error(), nil
	}
	return "", nil
}

type Operators interface {
	ResolveOperatorDidFor(ctx context.Context, did, service string) (string, bool, error)
}

func (b *Bidder) netRefused(ctx context.Context, rfp *market.RFP, requester string) (string, error) {
	payload, err := b.records.Resolve(ctx, market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return "", fmt.Errorf("bidder: resolving the RFP payload for its net: %w", err)
	}
	if net, _ := payload.Value["net"].(bool); !net {
		return "", nil
	}
	if kind, _ := payload.Value["kind"].(string); kind != string(v1alpha1.ComputeKindDeno) {
		return "net is a computeKind deno option", nil
	}
	return b.ownerOnly(ctx, requester)
}

func (b *Bidder) ownerOnly(ctx context.Context, requester string) (string, error) {
	if requester == b.cfg.DID {
		return "", nil
	}
	if b.operators == nil {
		return "the bidder cannot check its owner, so it runs no networked sandbox", nil
	}
	owner, found, err := b.operators.ResolveOperatorDidFor(ctx, b.cfg.DID, trust.BidderAssociate)
	if err != nil {
		return "", err
	}
	if !found {
		owner = b.cfg.DID
	}
	if requester == owner {
		return "", nil
	}
	operator, found, err := b.operators.ResolveOperatorDidFor(ctx, requester, trust.RequesterAssociate)
	if err != nil {
		return "", err
	}
	if found && operator == owner {
		return "", nil
	}
	return "a networked sandbox runs only for the bidder's owner " + owner, nil
}

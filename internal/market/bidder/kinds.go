package bidder

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
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
	record, err := parseVMRecord(payload.Value)
	if err != nil {
		return err.Error(), nil
	}
	if at, err := market.ParseTime(record.ExpiresAt); err == nil && !at.After(b.now()) {
		return fmt.Sprintf("bidder: the RFP payload's expiresAt %s has passed", record.ExpiresAt), nil
	}
	return b.sizeRefused(record), nil
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
	Walk(ctx context.Context, did string, assoc trust.Association) (identlink.Result, error)
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
	self, err := b.operators.Walk(ctx, b.cfg.DID, trust.Association{})
	if err != nil {
		return "", err
	}
	if slices.Contains(self.Roots, requester) {
		return "", nil
	}
	theirs, err := b.operators.Walk(ctx, requester, trust.Association{})
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(self.Roots, func(root string) bool { return slices.Contains(theirs.Roots, root) }) {
		return "", nil
	}
	owner := strings.Join(self.Roots, ", ")
	return "a networked sandbox runs only for the bidder's owner " + owner, nil
}

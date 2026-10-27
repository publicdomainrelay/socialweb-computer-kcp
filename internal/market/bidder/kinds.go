package bidder

import (
	"context"
	"fmt"
	"slices"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
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

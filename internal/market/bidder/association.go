package bidder

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const AssociationExpiryGrace = 10 * time.Minute

func (b *Bidder) associationExpiry(ctx context.Context, requesterDID, acceptedAt string) string {
	start := b.now()
	if at, err := market.ParseTime(acceptedAt); err == nil && at.Before(start) {
		start = at
	}
	bound := start.Add(market.ContractAssociationLifetime)
	if out := b.proofExpiry(ctx, requesterDID, bound); out != "" {
		return out
	}
	out := market.FormatTime(bound)
	b.log.Info("bidder_lifetime_capped", "requester", requesterDID, "expiresAt", out, "lifetime", market.ContractAssociationLifetime.String())
	return out
}

func (b *Bidder) proofExpiry(ctx context.Context, requesterDID string, bound time.Time) string {
	if b.records == nil || requesterDID == "" {
		return ""
	}
	skip := func(reason string, args ...any) string {
		b.log.Info("bidder_association_expiry_skipped", append([]any{"requester", requesterDID, "reason", reason}, args...)...)
		return ""
	}
	refuse := func(reason string, args ...any) string {
		b.log.Warn("bidder_association_expiry_refused", append([]any{"requester", requesterDID, "reason", reason}, args...)...)
		return ""
	}
	mirror, err := b.records.GetRecord(ctx, requesterDID, trust.BadgeBlueKeysNSID, trust.MirrorRKey)
	if errors.Is(err, market.ErrNotFound) {
		return skip("the requester has no keys mirror")
	}
	if err != nil {
		return refuse("the keys mirror is unread", "err", err.Error())
	}
	proof, _ := mirror.Value["proof"].(map[string]any)
	uri, _ := proof["uri"].(string)
	cid, _ := proof["cid"].(string)
	if uri == "" && cid == "" {
		return skip("the keys mirror names no proof")
	}
	if uri == "" || cid == "" || market.NSIDFromURI(uri) != market.AssociationNSID {
		return refuse("the keys mirror proof is no association strongRef", "proof", uri)
	}
	authority := market.ATURIAuthority(uri)
	if authority == "" || authority == requesterDID {
		return refuse("the proof is not in an authority's repo", "proof", uri)
	}
	authorities := []string{authority}
	subject := requesterDID
	var expiresAt time.Time
	var contract *market.StrongRef
	for depth := 0; ; depth++ {
		record, err := b.records.Resolve(ctx, market.RecordRef{URI: uri, CID: cid})
		if err != nil {
			return refuse("the proof is unread", "proof", uri, "err", err.Error())
		}
		if record.CID != cid {
			return refuse("the proof cid changed", "proof", uri)
		}
		assoc, err := market.ParseAssociation(record.Value)
		if err != nil {
			return refuse("the proof is no association", "proof", uri, "err", err.Error())
		}
		if assoc.Contract == nil {
			if depth == 0 {
				return skip("the proof is not contract-scoped", "proof", uri)
			}
			return refuse("a link of the association chain is not contract-scoped", "proof", uri)
		}
		if assoc.Subject != subject {
			return refuse("the proof names another subject", "proof", uri, "subject", assoc.Subject)
		}
		at, err := market.ParseTime(assoc.ExpiresAt)
		if err != nil {
			return refuse("the proof expiresAt is no datetime", "proof", uri, "expiresAt", assoc.ExpiresAt)
		}
		if expiresAt.IsZero() || at.Before(expiresAt) {
			expiresAt = at
		}
		if contract == nil {
			contract = assoc.Contract
		}
		if assoc.Parent == nil {
			break
		}
		if depth+1 >= trust.MaxChainDepth {
			return refuse("the association chain is deeper than its bound", "proof", uri)
		}
		subject = market.ATURIAuthority(uri)
		uri, cid = assoc.Parent.URI, assoc.Parent.CID
		parent := market.ATURIAuthority(uri)
		if market.NSIDFromURI(uri) != market.AssociationNSID || parent == "" || parent == requesterDID || slices.Contains(authorities, parent) {
			return refuse("the parent is no association of a repo outside the chain", "proof", uri)
		}
		authorities = append(authorities, parent)
	}
	if !slices.Contains(authorities, market.ATURIAuthority(contract.URI)) {
		return refuse("the proof's contract is not an accept of an authority of the chain", "proof", uri, "contract", contract.URI)
	}
	authority = authorities[len(authorities)-1]
	if bound.Before(expiresAt) {
		b.log.Info("bidder_association_expiry_capped", "requester", requesterDID, "proof", uri, "expiresAt", market.FormatTime(expiresAt), "cap", market.FormatTime(bound))
		expiresAt = bound
	}
	out := market.FormatTime(expiresAt)
	b.log.Info("bidder_association_expiry", "requester", requesterDID, "proof", uri, "authority", authority, "expiresAt", out)
	return out
}

func (b *Bidder) releaseAtAssociationExpiry(ctx context.Context, vm *v1alpha1.ComputeVM) time.Duration {
	raw := vm.Spec.AssociationExpiresAt
	if raw == "" || vm.Spec.Release != nil || vm.DeletionTimestamp != nil || b.guests.Ended(vm) {
		return 0
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return 0
	}
	deadline := expiresAt.Add(AssociationExpiryGrace)
	if wait := deadline.Sub(b.now()); wait > 0 {
		return wait
	}
	release := v1alpha1.ComputeVMRelease{RequestedAt: b.now().UTC().Format(time.RFC3339)}
	if err := b.intents.ApplyComputeVMRelease(ctx, vm.Name, release); err != nil {
		b.log.Warn("bidder_association_expired_release_failed", "computevm", vm.Name, "err", err.Error())
		return time.Minute
	}
	b.log.Info("bidder_association_expired_release", "computevm", vm.Name, "requester", vm.Spec.Requester.Did, "associationExpiresAt", raw)
	return 0
}

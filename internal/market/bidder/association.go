package bidder

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const AssociationExpiryGrace = 10 * time.Minute

func (b *Bidder) associationExpiry(ctx context.Context, requesterDID string) string {
	if b.records == nil || requesterDID == "" {
		return ""
	}
	mirror, err := b.records.GetRecord(ctx, requesterDID, trust.BadgeBlueKeysNSID, trust.MirrorRKey)
	if err != nil {
		return ""
	}
	proof, _ := mirror.Value["proof"].(map[string]any)
	uri, _ := proof["uri"].(string)
	cid, _ := proof["cid"].(string)
	if uri == "" || cid == "" || market.NSIDFromURI(uri) != market.AssociationNSID {
		return ""
	}
	record, err := b.records.Resolve(ctx, market.RecordRef{URI: uri, CID: cid})
	if err != nil {
		b.log.Warn("bidder_association_expiry_unread", "requester", requesterDID, "proof", uri, "err", err.Error())
		return ""
	}
	if record.CID != cid {
		b.log.Warn("bidder_association_expiry_unread", "requester", requesterDID, "proof", uri, "err", "the proof cid changed")
		return ""
	}
	if _, scoped := record.Value["contract"]; !scoped {
		return ""
	}
	expiresAt, _ := record.Value["expiresAt"].(string)
	if _, err := time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		return ""
	}
	b.log.Info("bidder_association_expiry", "requester", requesterDID, "proof", uri, "expiresAt", expiresAt)
	return expiresAt
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

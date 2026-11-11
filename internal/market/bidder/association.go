package bidder

import (
	"context"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const AssociationExpiryGrace = 10 * time.Minute

func (b *Bidder) associationExpiry(ctx context.Context, requesterDID, acceptedAt, vmExpiresAt string) string {
	start := b.now()
	if at, err := market.ParseTime(acceptedAt); err == nil && at.Before(start) {
		start = at
	}
	bound, source := start.Add(market.ContractAssociationLifetime), "accept + "+market.ContractAssociationLifetime.String()
	if at, err := market.ParseTime(vmExpiresAt); err == nil {
		bound, source = at, "compute.vm expiresAt"
	}
	if b.operators != nil && requesterDID != "" {
		walked, err := b.operators.Walk(ctx, requesterDID, trust.Association{})
		switch {
		case err != nil:
			b.log.Warn("bidder_association_expiry_unread", "requester", requesterDID, "err", err.Error())
		case walked.ExpiresAt != nil && walked.ExpiresAt.Before(bound):
			bound, source = *walked.ExpiresAt, "identlink lifetime of "+strings.Join(walked.Roots, ", ")
		}
	}
	out := market.FormatTime(bound)
	b.log.Info("bidder_lifetime_capped", "requester", requesterDID, "expiresAt", out, "source", source)
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

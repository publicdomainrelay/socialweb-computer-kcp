package bidder

import (
	"context"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

func (b *Bidder) report(ctx context.Context, vm *v1alpha1.ComputeVM) {
	if vm.Spec.Owner.Handle != b.cfg.ProviderHandle || vm.DeletionTimestamp != nil || b.guests.Ended(vm) {
		return
	}
	terms := vm.Spec.Contract
	if terms == nil || terms.Receipt.URI == "" || terms.Receipt.CID == "" {
		return
	}
	held := b.guests.GuestReport(vm)
	if held == nil {
		return
	}
	published := v1alpha1.ComputeVMPublished{}
	if p := b.guests.Published(vm); p != nil {
		published = *p
	}
	now := b.now().UTC().Format(time.RFC3339)
	if held.Address != "" && published.OnNetworkURI == "" {
		record := map[string]any{
			"$type":     market.ComputeEventsVMOnNetworkNSID,
			"address":   held.Address,
			"createdAt": firstOf(held.ReportedAt, now),
		}
		published = b.publishReport(ctx, vm, market.ComputeEventsVMOnNetworkNSID, record, published)
	}
	if len(held.HostKeys) > 0 && published.HostKeysURI == "" {
		keys := make([]any, 0, len(held.HostKeys))
		for _, key := range held.HostKeys {
			keys = append(keys, map[string]any{"type": key.Type, "publicKey": key.PublicKey, "fingerprint": key.Fingerprint})
		}
		record := map[string]any{
			"$type":      market.ComputeEventsVMHostKeysNSID,
			"keys":       keys,
			"observedAt": firstOf(held.ReportedAt, now),
			"createdAt":  now,
		}
		b.publishReport(ctx, vm, market.ComputeEventsVMHostKeysNSID, record, published)
	}
}

func firstOf(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (b *Bidder) publishReport(ctx context.Context, vm *v1alpha1.ComputeVM, collection string, record map[string]any, report v1alpha1.ComputeVMPublished) v1alpha1.ComputeVMPublished {
	name, err := b.ensureReportSync(ctx, vm, collection, record)
	if err != nil {
		b.log.Warn("ensuring a market report StateSync failed", "computevm", vm.Name, "collection", collection, "err", err)
		return report
	}
	sync, ok := b.syncs.Get(name)
	if !ok || sync.Status.Phase != v1alpha1.StateSyncPhaseAcked || sync.Status.Acked == nil {
		return report
	}
	uri := strings.TrimSpace(sync.Status.Acked.Ref)
	if uri == "" {
		return report
	}
	if collection == market.ComputeEventsVMOnNetworkNSID {
		report.OnNetworkURI = uri
	} else {
		report.HostKeysURI = uri
	}
	if err := b.intents.ApplyComputeVMPublished(ctx, vm.Name, report); err != nil {
		if ctx.Err() == nil {
			b.log.Warn("recording a ComputeVM market report URI failed", "computevm", vm.Name, "err", err)
		}
		return report
	}
	b.log.Info("a ComputeVM market report is recorded", "computevm", vm.Name, "collection", collection, "payload", uri)
	return report
}

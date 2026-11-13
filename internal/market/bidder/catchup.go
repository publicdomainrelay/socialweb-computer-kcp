package bidder

import (
	"context"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	catchUpEvents = 50

	catchUpTimeout = 2 * time.Minute
)

func (b *Bidder) catchUpAll() {
	for _, vm := range b.vms.List() {
		b.catchUp(vm)
	}
}

// ponytail: once per live VM per process (start, restart, crash, or the VM first seen), the newest catchUpEvents market.event records of its requester; a stream gap inside one process with a stored cursor older than delivery.RecentResumeWindow is not read again
func (b *Bidder) catchUp(vm v1alpha1.ComputeVM) {
	if vm.Spec.Contract == nil || !b.liveGuest(vm) {
		return
	}
	receipt, requester := strings.TrimSpace(vm.Spec.Contract.Receipt.URI), strings.TrimSpace(vm.Spec.Requester.Did)
	if receipt == "" || requester == "" {
		return
	}
	if _, done := b.caughtUp.LoadOrStore(receipt, true); done {
		return
	}
	b.wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), catchUpTimeout)
		defer cancel()
		page, err := b.records.ListRecords(ctx, requester, market.EventNSID, catchUpEvents, "")
		if err != nil {
			b.caughtUp.Delete(receipt)
			b.log.Warn("bidder_catchup_failed", "computevm", vm.Name, "requester", requester, "err", err.Error())
			return
		}
		applied := 0
		for _, listed := range page.Records {
			event, err := market.ParseEvent(listed.Value)
			if err != nil || event.Receipt.URI != receipt {
				continue
			}
			resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: listed.URI, CID: listed.CID})
			if err != nil {
				b.log.Warn("bidder_catchup_event_unread", "computevm", vm.Name, "event", listed.URI, "err", err.Error())
				continue
			}
			if err := b.applyMarketEvent(ctx, listed.URI, listed.CID, resolved.Value, requester); err != nil {
				b.log.Warn("bidder_catchup_event_refused", "computevm", vm.Name, "event", listed.URI, "err", err.Error())
				continue
			}
			applied++
		}
		b.log.Info("bidder_catchup", "computevm", vm.Name, "requester", requester, "listed", len(page.Records), "applied", applied)
	})
}

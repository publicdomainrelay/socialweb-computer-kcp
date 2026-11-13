package bidder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
)

var errGuestEventsUnavailable = errors.New("bidder: the guest event queue is not available")

func (b *Bidder) PublishGuestEvent(ctx context.Context, event guestcp.GuestEvent) error {
	if !market.IsGuestEventNSID(event.Collection) {
		return fmt.Errorf("bidder: %q is not a guest event collection", event.Collection)
	}
	held, ok := b.vms.Get(event.ComputeVM)
	if !ok {
		return fmt.Errorf("bidder: the ComputeVM %s is not known", event.ComputeVM)
	}
	vm := &held
	if vm.Spec.Owner.Handle != b.cfg.ProviderHandle || vm.DeletionTimestamp != nil || b.guests.Ended(vm) || b.guests.Releasing(vm) {
		return fmt.Errorf("bidder: the ComputeVM %s is not a live guest of this bidder", vm.Name)
	}
	record := map[string]any{}
	if err := json.Unmarshal(event.Record, &record); err != nil {
		return fmt.Errorf("bidder: the guest event record is not JSON: %w", err)
	}
	if kind, _ := record["$type"].(string); kind != event.Collection {
		return fmt.Errorf("bidder: the guest event record $type %q is not %s", kind, event.Collection)
	}
	if market.IsGuestMemoryEventNSID(event.Collection) {
		if why := b.memoryEventCapped(vm, event.Collection, event.Tag, event.Kind); why != "" {
			b.log.Warn("a guest memory event is dropped", "computevm", vm.Name, "collection", event.Collection, "kind", event.Kind, "why", why)
			return nil
		}
	}
	if event.Collection == market.ComputeEventsGuestImageNSID && b.guestEventSent(vm, event.Collection) {
		b.log.Warn("a second guest.image event is dropped", "computevm", vm.Name)
		return nil
	}
	name, err := b.ensureGuestEventSync(ctx, vm, event.Collection, event.Tag, event.Kind, record)
	if err != nil {
		return err
	}
	b.log.Info("a guest event is published to the requester", "computevm", vm.Name, "collection", event.Collection,
		"tag", event.Tag, "kind", event.Kind, "stateSync", name)
	return nil
}

func (b *Bidder) guestEventSent(vm *v1alpha1.ComputeVM, collection string) bool {
	for _, sync := range b.syncs.List() {
		if b.guests.SyncVM(&sync) == vm.Name && strings.HasPrefix(sync.Spec.IdempotencyKey, guestEventSyncKeyPrefix+collection+":") {
			return true
		}
	}
	return false
}

func (b *Bidder) memoryEventCapped(vm *v1alpha1.ComputeVM, collection, tag, kind string) string {
	total, last := 0, time.Time{}
	stream := guestEventStream(collection, tag, kind)
	for _, sync := range b.syncs.List() {
		if b.guests.SyncVM(&sync) != vm.Name {
			continue
		}
		key := sync.Spec.IdempotencyKey
		if strings.HasPrefix(key, guestEventSyncKeyPrefix+market.ComputeEventsGuestMemoryPressureNSID+":") ||
			strings.HasPrefix(key, guestEventSyncKeyPrefix+market.ComputeEventsGuestOOMKillNSID+":") {
			total++
		}
		if strings.HasPrefix(key, stream) && sync.CreationTimestamp.After(last) {
			last = sync.CreationTimestamp.Time
		}
	}
	switch {
	case total >= market.GuestMemoryEventsPerContract:
		return fmt.Sprintf("the contract had its %d memory events", market.GuestMemoryEventsPerContract)
	case time.Since(last) < market.GuestMemoryEventEvery:
		return fmt.Sprintf("one of this kind went out less than %s ago", market.GuestMemoryEventEvery)
	}
	return ""
}

func (b *Bidder) applyGuestEvent(ctx context.Context, uri, cid string, record map[string]any, event *market.Event, issuer string) error {
	if b.releasedByReceipt(event.Receipt.URI) {
		return nil
	}
	info, vm, found := b.localContractForReceipt(event.Receipt.URI)
	if !found {
		return fmt.Errorf("bidder: no ComputeVM holds the receipt %s yet", event.Receipt.URI)
	}
	if info.acceptAuthor == "" || info.acceptAuthor != issuer || market.ATURIAuthority(uri) != issuer {
		return ErrEventForbidden
	}
	if vm.DeletionTimestamp != nil || b.guests.Ended(vm) || b.guests.Releasing(vm) {
		return nil
	}
	if event.Receipt.CID != info.receiptCID {
		return fmt.Errorf("%w: the event names another receipt", ErrEventInvalid)
	}
	if market.ATURIAuthority(event.Payload.URI) != issuer {
		return fmt.Errorf("%w: the payload is not the requester's", ErrEventInvalid)
	}
	if err := b.verifier.VerifyRecord(ctx, market.ResolvedRecord{URI: uri, CID: cid, Value: record}); err != nil {
		return fmt.Errorf("%w: the event is not signed: %v", ErrEventInvalid, err)
	}
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: event.Payload.URI, CID: event.Payload.CID})
	if err != nil {
		return fmt.Errorf("bidder: resolving the guest event payload: %w", err)
	}
	if resolved.CID != event.Payload.CID {
		return fmt.Errorf("%w: the payload CID %s is not the event's %s", ErrEventInvalid, resolved.CID, event.Payload.CID)
	}
	nsid, err := b.guests.CheckGuestEvent(resolved.Value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEventInvalid, err)
	}
	if nsid != market.NSIDFromURI(event.Payload.URI) {
		return fmt.Errorf("%w: the payload $type %s is not its collection", ErrEventInvalid, nsid)
	}
	if !market.IsRequesterGuestEventNSID(nsid) {
		return fmt.Errorf("%w: %s is the guest's to send, not the requester's", ErrEventInvalid, nsid)
	}
	if err := b.verifier.VerifyRecord(ctx, resolved); err != nil {
		return fmt.Errorf("%w: the payload is not signed: %v", ErrEventInvalid, err)
	}
	if b.guestEvents == nil {
		return errGuestEventsUnavailable
	}
	raw, err := json.Marshal(resolved.Value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEventInvalid, err)
	}
	appended, err := b.guestEvents.Append(ctx, vm.Name, guestcp.QueuedEvent{URI: resolved.URI, CID: resolved.CID, Record: raw})
	if err != nil {
		return fmt.Errorf("bidder: queueing the guest event: %w", err)
	}
	if appended {
		b.log.Info("a requester guest event is queued for the guest", "computevm", vm.Name, "collection", nsid, "payload", resolved.URI)
	}
	return nil
}

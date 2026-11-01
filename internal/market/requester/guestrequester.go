package requester

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const GuestRequesterReleaseTimeout = 12 * time.Minute

func GuestRequesterReleaseEventName(contract string) string {
	name := "r2-release-" + contract
	if len(name) > 253 {
		return name[:253]
	}
	return name
}

func (r *contractRunner) releaseGuestRequester(ctx context.Context, state *v1alpha1.NodeClaimContract) {
	q := r.q
	contract := r.snapshot()
	if !fromGuest(contract) || state == nil || state.GuestRequester == nil || state.GuestRequester.State != v1alpha1.ContractGuestRequesterVerified ||
		q.deps.ContractEvents == nil || contract.UID == "" {
		return
	}
	receiptURI := receiptURIOf(state)
	if receiptURI == "" {
		return
	}
	events, pending, closeEvents := q.registerEventSink(receiptURI)
	defer closeEvents()
	name := GuestRequesterReleaseEventName(contract.Name)
	event := v1alpha1.ContractEvent{
		ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{contractOwnerReference(contract)}},
		Spec: v1alpha1.ContractEventSpec{
			ContractRef: commonv1.LocalObjectRef{Name: contract.Name},
			Direction:   v1alpha1.ContractEventOutbound,
			NSID:        market.ComputeEventsGuestR2ReleaseNSID,
		},
	}
	if err := q.deps.ContractEvents.ApplyContractEvent(ctx, event); err != nil {
		r.log().Warn("requester_guest_requester_release_failed", "step", "the r2Release ContractEvent", "err", err.Error())
		return
	}
	q.contractEvents.Put(name, event)
	r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "asking the guest requester to release its own contracts before the guest goes", state,
		r.currentBids(), r.currentChosen(), "releasingGuestRequester", 1)
	r.log().Info("requester_guest_requester_release", "contractEvent", name, "timeout", GuestRequesterReleaseTimeout.String())
	answered := func(e MarketEvent) bool {
		if market.NSIDFromURI(e.Payload.URI) != market.ComputeEventsGuestR2ReleasedNSID {
			return false
		}
		parsed, err := q.parseGuestEvent(ctx, e, receiptURI)
		if err != nil || parsed.R2Released == nil {
			if err != nil {
				r.log().Warn("requester_guest_event_refused", "uri", e.Event.URI, "err", err)
			}
			return false
		}
		r.log().Info("requester_guest_requester_released", "ok", *parsed.R2Released, "message", parsed.R2Message)
		return true
	}
	for _, e := range pending {
		if answered(e) {
			return
		}
	}
	timer := time.NewTimer(GuestRequesterReleaseTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.log().Warn("requester_guest_requester_release_timeout", "after", GuestRequesterReleaseTimeout.String(),
				"next", "the guest is destroyed; the guest requester's bidders end its contracts when its association goes")
			return
		case e := <-events:
			if answered(e) {
				return
			}
		}
	}
}

package requester

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const GuestRequesterReleaseTimeout = cloudinit.GuestRequesterReleaseTimeout

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
	if !fromGuest(contract) || state == nil || state.GuestRequester == nil ||
		(state.GuestRequester.State != v1alpha1.ContractGuestRequesterVerified && state.GuestRequester.State != v1alpha1.ContractGuestRequesterPending) ||
		q.deps.ContractEvents == nil || contract.UID == "" {
		return
	}
	if state.R2Release != nil && state.R2Release.Phase != v1alpha1.ContractR2ReleaseRequested {
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
	if state.R2Release == nil || state.R2Release.Phase != v1alpha1.ContractR2ReleaseRequested {
		state.R2Release = &v1alpha1.ContractR2Release{Phase: v1alpha1.ContractR2ReleaseRequested, RequestedAt: q.nowISO()}
	}
	settle := func(phase v1alpha1.ContractR2ReleasePhase, ok bool, message string) {
		state.R2Release.Phase, state.R2Release.Ok, state.R2Release.Message, state.R2Release.AnsweredAt = phase, ok, message, q.nowISO()
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the guest requester's release: "+string(phase)+" "+message, state,
			r.currentBids(), r.currentChosen(), "releasingGuestRequester", 1)
	}
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
		phase := v1alpha1.ContractR2ReleaseReleased
		if !*parsed.R2Released {
			phase = v1alpha1.ContractR2ReleaseFailed
		}
		settle(phase, *parsed.R2Released, parsed.R2Message)
		return true
	}
	for _, e := range pending {
		if answered(e) {
			return
		}
	}
	wait := GuestRequesterReleaseTimeout
	if at, err := market.ParseTime(state.R2Release.RequestedAt); err == nil {
		wait = max(at.Add(GuestRequesterReleaseTimeout).Sub(q.now()), 0)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.log().Warn("requester_guest_requester_release_timeout", "after", GuestRequesterReleaseTimeout.String(),
				"next", "the guest is destroyed; the guest requester's bidders end its contracts when its association goes")
			settle(v1alpha1.ContractR2ReleaseTimedOut, false, "no guest.r2Released within "+GuestRequesterReleaseTimeout.String()+
				"; the guest requester's bidders release its VMs at its association expiresAt plus 10m")
			return
		case e := <-events:
			if answered(e) {
				return
			}
		}
	}
}

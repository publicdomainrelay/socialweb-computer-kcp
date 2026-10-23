package bidder

import (
	"context"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	BidTTL = "24h"

	WonUnboundWait = time.Hour
)

func bidFinished(bid *v1alpha1.Bid) bool {
	for _, t := range []string{v1alpha1.BidConditionComplete, v1alpha1.BidConditionFailed} {
		if c := apimeta.FindStatusCondition(bid.Status.Conditions, t); c != nil && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func bidFinish(bid *v1alpha1.Bid, vmLive func(string) bool, now time.Time) (string, time.Time, string, bool) {
	finished := func() time.Time {
		if at, err := time.Parse(time.RFC3339, bid.Status.Timings.FinishedAt); err == nil {
			return at
		}
		return bid.CreationTimestamp.Time
	}
	switch bid.Status.Phase {
	case v1alpha1.BidPhaseLost, v1alpha1.BidPhaseExpired:
		return v1alpha1.BidConditionComplete, finished(), string(bid.Status.Phase), true
	case v1alpha1.BidPhaseFailed:
		return v1alpha1.BidConditionFailed, finished(), string(bid.Status.Phase), true
	case v1alpha1.BidPhaseWon:
		if vm := bid.Status.ComputeVM; vm != "" {
			if vmLive(vm) {
				return "", time.Time{}, "", false
			}
			return v1alpha1.BidConditionComplete, now, "ComputeVMGone", true
		}
		at, err := time.Parse(time.RFC3339, bid.Status.Timings.AcceptedAt)
		if err != nil || now.Sub(at) < WonUnboundWait {
			return "", time.Time{}, "", false
		}
		return v1alpha1.BidConditionComplete, now, "NeverBound", true
	}
	return "", time.Time{}, "", false
}

func (s *Service) bindWonBid(ctx context.Context, bid *v1alpha1.Bid) {
	if bid.Status.Phase != v1alpha1.BidPhaseWon || bid.Status.ComputeVM != "" || bid.Status.Accept == nil || s.vms == nil {
		return
	}
	vm, err := s.vmForAccept(ctx, bid.Status.Accept.URI, bid.Status.Accept.CID)
	if err != nil || vm == nil {
		return
	}
	status := cloneBidStatus(bid.Status)
	status.ComputeVM = vm.Name
	if s.applyBidStatus(ctx, bid, status) {
		s.log.Info("a Won Bid is bound to the ComputeVM that serves its accept", "bid", bid.Name, "computevm", vm.Name)
	}
}

func (s *Service) finishBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) {
	if bidFinished(bid) {
		return
	}
	live := func(name string) bool {
		vm, err := s.vms.Get(ctx, name)
		return err != nil || vm != nil
	}
	kind, at, reason, done := bidFinish(bid, live, now)
	if !done {
		return
	}
	status := cloneBidStatus(bid.Status)
	conds.SetStatusAt(&status.Conditions, bid.Generation, kind, metav1.ConditionTrue, reason, "the Bid is finished; its TTL runs from now", at)
	if s.applyBidStatus(ctx, bid, status) {
		s.log.Info("a finished Bid carries its condition; the TTL controller removes it after its TTL", "bid", bid.Name, "ttl", BidTTL)
	}
}

func bidOwner(bid *v1alpha1.Bid) *metav1.OwnerReference {
	if bid == nil || bid.UID == "" {
		return nil
	}
	return &metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Bid", Name: bid.Name, UID: bid.UID}
}

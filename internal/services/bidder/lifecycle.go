package bidder

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

const (
	DefaultBidReconcileInterval = 30 * time.Second

	PublishStallThreshold = 3

	StatusMessageLimit = 512

	BidConditionReleasing = "Releasing"
)

func (s *Service) reconcileBidsLoop(ctx context.Context) {
	s.reconcileBids(ctx)
	ticker := time.NewTicker(DefaultBidReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileBids(ctx)
		}
	}
}

func (s *Service) reconcileBids(ctx context.Context) {
	if s.bids == nil {
		return
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("listing the bids for reconciliation failed", "err", err)
		}
		return
	}
	now := s.cfg.Now().UTC()
	for i := range bids {
		s.reconcileBid(ctx, &bids[i], now)
	}
}

func (s *Service) reconcileBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	switch bid.Status.Phase {
	case v1alpha1.BidPhaseDeciding, v1alpha1.BidPhasePublished:
	default:
		return
	}
	if bidExpired(bid, now) {
		s.expireBid(ctx, bid, now)
		return
	}
	if bid.Status.Phase == v1alpha1.BidPhasePublished {
		if _, ok := s.verifiedBids.Load(bid.Name); ok {
			return
		}
		ref, found, err := s.existingBidRecord(ctx, bid)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("reading the published bid record failed", "bid", bid.Name, "err", err)
			}
			return
		}
		if found {
			s.recordPublishedBid(ctx, bid, ref, now)
			s.verifiedBids.Store(bid.Name, true)
			return
		}
	}
	if s.records == nil || s.attestation == nil || s.did == "" {
		return
	}
	rfp := s.rfpForBid(ctx, bid)
	if err := s.publishBidRecord(ctx, bid, rfp, now, true); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("resuming the bid publish failed", "bid", bid.Name, "err", err)
		}
	}
}

func bidExpired(bid *v1alpha1.Bid, now time.Time) bool {
	raw := strings.TrimSpace(bid.Spec.ExpiresAt)
	if raw == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return !now.Before(expires)
}

func (s *Service) expireBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) {
	if bid.Status.Phase != v1alpha1.BidPhaseDeciding && bid.Status.Phase != v1alpha1.BidPhasePublished {
		return
	}
	status := cloneBidStatus(bid.Status)
	status.Phase = v1alpha1.BidPhaseExpired
	status.Timings.FinishedAt = now.Format(time.RFC3339)
	setBidCondition(&status, bid.Generation, v1alpha1.BidConditionExpired, metav1.ConditionTrue, "Deadline",
		"the bid expired at "+bid.Spec.ExpiresAt)
	s.applyBidStatus(ctx, bid, status)
	s.log.Info("a bid expired", "bid", bid.Name, "expiresAt", bid.Spec.ExpiresAt)
}

func (s *Service) publishBidRecord(ctx context.Context, bid *v1alpha1.Bid, rfp *market.RFP, now time.Time, resume bool) error {
	var ref market.StrongRef
	found := false
	if resume {
		existing, ok, err := s.existingBidRecord(ctx, bid)
		if err != nil {
			return err
		}
		ref, found = existing, ok
	}
	if !found {
		published, err := s.writeBidRecord(ctx, bid, now)
		if err != nil {
			s.recordPublishFailure(ctx, bid, err)
			return err
		}
		ref = published
	}
	changed := s.recordPublishedBid(ctx, bid, ref, now)
	s.verifiedBids.Store(bid.Name, true)
	if changed && rfp != nil && rfp.SubmitBid != "" {
		s.submitBid(ctx, rfp.SubmitBid, ref)
	}
	return nil
}

func (s *Service) recordPublishFailure(ctx context.Context, bid *v1alpha1.Bid, err error) {
	status := cloneBidStatus(bid.Status)
	status.PublishAttempts++
	status.LastError = statusError(err)
	if status.PublishAttempts >= PublishStallThreshold {
		setBidCondition(&status, bid.Generation, v1alpha1.BidConditionStalled, metav1.ConditionTrue, "PublishFailed", status.LastError)
	}
	s.applyBidStatus(ctx, bid, status)
}

func (s *Service) recordPublishedBid(ctx context.Context, bid *v1alpha1.Bid, ref market.StrongRef, now time.Time) bool {
	status := cloneBidStatus(bid.Status)
	if status.Phase == v1alpha1.BidPhasePublished && status.Bid != nil && status.Bid.Uri == ref.URI && status.Bid.Cid == ref.CID && status.LastError == "" {
		return false
	}
	status.Phase = v1alpha1.BidPhasePublished
	status.Bid = &v1alpha1.BidRecordRef{Uri: ref.URI, Cid: ref.CID}
	if status.Timings.PublishedAt == "" {
		status.Timings.PublishedAt = now.Format(time.RFC3339)
	}
	status.LastError = ""
	setBidCondition(&status, bid.Generation, v1alpha1.BidConditionPublished, metav1.ConditionTrue, "Published", "the bid record is published")
	setBidCondition(&status, bid.Generation, v1alpha1.BidConditionStalled, metav1.ConditionFalse, "Published", "the bid publish recovered")
	return s.applyBidStatus(ctx, bid, status)
}

func (s *Service) writeBidRecord(ctx context.Context, bid *v1alpha1.Bid, now time.Time) (market.StrongRef, error) {
	if s.records == nil || s.attestation == nil {
		return market.StrongRef{}, errors.New("the bidder has no record store or attestation key")
	}
	rkey := bidRecordRkey(bid)
	config, err := guestidentity.BidConfigFor(s.guestURL, s.did, s.cfg.Provider.AcceptPath, now.Format(time.RFC3339))
	if err != nil {
		return market.StrongRef{}, err
	}
	configRecord, err := structMap(config)
	if err != nil {
		return market.StrongRef{}, err
	}
	configRef, err := s.records.PutRecord(ctx, s.did, guestidentity.WIFSimpleNSID, rkey+"-config", configRecord)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid config: %w", err)
	}
	payloadRecord, err := market.SignRecord(map[string]any{
		"$type":     market.BidsFreeNSID,
		"reason":    "free",
		"cost":      0,
		"createdAt": now.Format(time.RFC3339),
	}, s.did, s.attestation)
	if err != nil {
		return market.StrongRef{}, err
	}
	payload, err := s.records.PutRecord(ctx, s.did, market.BidsFreeNSID, rkey+"-payload", payloadRecord)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid payload: %w", err)
	}
	record, err := market.BuildBid(market.BidOptions{
		RFP:          market.RecordRef{URI: bid.Spec.Rfp.Uri, CID: bid.Spec.Rfp.Cid},
		Config:       &market.RecordRef{URI: configRef.URI, CID: configRef.CID},
		Payload:      market.RecordRef{URI: payload.URI, CID: payload.CID},
		SubmitAccept: s.publicURL(),
		CreatedAt:    now.Format(time.RFC3339),
	})
	if err != nil {
		return market.StrongRef{}, err
	}
	signed, err := market.SignRecord(record, s.did, s.attestation)
	if err != nil {
		return market.StrongRef{}, err
	}
	ref, err := s.records.PutRecord(ctx, s.did, market.BidNSID, rkey, signed)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid: %w", err)
	}
	s.log.Info("the bidder published a bid", "rfp", bid.Spec.Rfp.Uri, "bid", ref.URI)
	return ref, nil
}

func (s *Service) existingBidRecord(ctx context.Context, bid *v1alpha1.Bid) (market.StrongRef, bool, error) {
	if s.records == nil || s.did == "" {
		return market.StrongRef{}, false, nil
	}
	if bid.Status.Bid != nil && bid.Status.Bid.Uri != "" {
		rec, err := s.records.Resolve(ctx, market.RecordRef{URI: bid.Status.Bid.Uri, CID: bid.Status.Bid.Cid})
		switch {
		case err == nil:
			return market.NewStrongRef(rec.URI, rec.CID), true, nil
		case errors.Is(err, market.ErrNotFound):
		default:
			return market.StrongRef{}, false, err
		}
	}
	rec, err := s.records.GetRecord(ctx, s.did, market.BidNSID, bidRecordRkey(bid))
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return market.StrongRef{}, false, nil
		}
		return market.StrongRef{}, false, err
	}
	return market.NewStrongRef(rec.URI, rec.CID), true, nil
}

func (s *Service) rfpForBid(ctx context.Context, bid *v1alpha1.Bid) *market.RFP {
	if s.records == nil || bid.Spec.Rfp.Uri == "" {
		return nil
	}
	resolved, err := s.records.Resolve(ctx, market.RecordRef{URI: bid.Spec.Rfp.Uri, CID: bid.Spec.Rfp.Cid})
	if err != nil {
		return nil
	}
	rfp, err := market.ParseRFP(resolved.Value)
	if err != nil {
		return nil
	}
	return rfp
}

func (s *Service) applyBidStatus(ctx context.Context, bid *v1alpha1.Bid, status v1alpha1.BidStatus) bool {
	if reflect.DeepEqual(status, bid.Status) {
		return false
	}
	if s.bids == nil {
		return false
	}
	if err := s.bids.PatchStatus(ctx, bid.Name, status); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("patching the bid status failed", "bid", bid.Name, "err", err)
		}
		return false
	}
	bid.Status = status
	return true
}

func (s *Service) markBidReleasing(ctx context.Context, bid *v1alpha1.Bid, info contractInfo, reason, message string) {
	if bid == nil {
		return
	}
	now := s.cfg.Now().UTC()
	status := cloneBidStatus(bid.Status)
	status.Terminated = &v1alpha1.BidTerminated{
		ReceiptUri:   info.receiptURI,
		ReceiptCid:   info.receiptCID,
		AcceptAuthor: info.acceptAuthor,
		Reason:       "deleted_on_request",
		At:           now.Format(time.RFC3339),
	}
	setBidCondition(&status, bid.Generation, BidConditionReleasing, metav1.ConditionTrue, reason, message)
	s.applyBidStatus(ctx, bid, status)
}

func cloneBidStatus(status v1alpha1.BidStatus) v1alpha1.BidStatus {
	if status.Conditions != nil {
		status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	}
	return status
}

func setBidCondition(status *v1alpha1.BidStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

func statusError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if len(text) > StatusMessageLimit {
		text = text[:StatusMessageLimit]
	}
	return text
}

func bidRecordRkey(bid *v1alpha1.Bid) string {
	return bid.Name
}

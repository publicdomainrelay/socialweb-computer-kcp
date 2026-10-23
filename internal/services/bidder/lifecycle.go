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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"
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
		case <-s.bidKick:
			s.reconcileBids(ctx)
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
	if bid.Status.Terminated != nil && bid.Status.Phase == v1alpha1.BidPhaseWon {
		s.finishRelease(ctx, bid, now)
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
	var rfp *market.RFP
	if bid.Status.Phase == v1alpha1.BidPhaseDeciding {
		if bid.Spec.PayloadNsid == "" || bid.Spec.Policy.Verdict != "allow" {
			rfp = s.validateBid(ctx, bid, now)
			if rfp == nil {
				return
			}
			s.applyBidSpec(ctx, bid, rfp, now)
		} else {
			rfp = s.rfpForBid(ctx, bid)
		}
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
	if rfp == nil {
		rfp = s.rfpForBid(ctx, bid)
	}
	if err := s.publishBidRecord(ctx, bid, rfp, now, true); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("resuming the bid publish failed", "bid", bid.Name, "err", err)
		}
	}
}

func (s *Service) validateBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) *market.RFP {
	if s.records == nil || s.attestation == nil || s.did == "" {
		return nil
	}
	resolved, err := s.records.Resolve(ctx, market.RecordRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("resolving the RFP of a bid failed", "bid", bid.Name, "err", err)
		}
		return nil
	}
	rfp, err := market.ParseRFP(resolved.Value)
	if err != nil {
		s.failBid(ctx, bid, "InvalidRFP", err.Error(), now)
		return nil
	}
	if err := s.verifyRecord(ctx, resolved); err != nil {
		s.failBid(ctx, bid, "InvalidSignature", err.Error(), now)
		return nil
	}
	if scope := s.policy.Scope(ctx, PolicyInput{SelfDid: s.did, CounterpartyDid: bid.Spec.Requester}); !scope.Allow {
		s.failBid(ctx, bid, "ScopeDenied", scope.Reason, now)
		return nil
	}
	decision := s.policy.EvaluatePolicies(ctx, rfp.Policies, PolicyInput{
		Perspective:      PolicyPerspectiveBidder,
		SelfDid:          s.did,
		SubjectDid:       s.did,
		CounterpartyDid:  bid.Spec.Requester,
		RootRequesterDid: bid.Spec.Requester,
		RFP:              &PolicyStrongRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID},
		Mode:             PolicyModeFull,
	})
	if !decision.Allow {
		s.failBid(ctx, bid, "PolicyRejected", decision.Reason, now)
		return nil
	}
	return rfp
}

func (s *Service) applyBidSpec(ctx context.Context, bid *v1alpha1.Bid, rfp *market.RFP, now time.Time) {
	attached := make([]v1alpha1.BidRecordRef, 0, len(rfp.Policies))
	for _, policy := range rfp.Policies {
		attached = append(attached, v1alpha1.BidRecordRef{URI: policy.URI, CID: policy.CID})
	}
	spec := bid.Spec
	spec.PayloadNsid = market.NSIDFromURI(rfp.Payload.URI)
	spec.Policy = v1alpha1.BidPolicy{Attached: attached, Verdict: "allow"}
	if spec.Offer.Kind == "" {
		spec.Offer = v1alpha1.BidOffer{Kind: "free"}
	}
	if spec.Provider == "" {
		spec.Provider = "kcp"
	}
	spec.ExpiresAt = now.Add(s.cfg.bidTTL()).Format(time.RFC3339)
	if err := s.bids.PatchSpec(ctx, bid.Name, spec); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("recording the bid decision failed", "bid", bid.Name, "err", err)
		}
		return
	}
	bid.Spec = spec
	status := cloneBidStatus(bid.Status)
	if status.Timings.DecidedAt == "" {
		status.Timings.DecidedAt = now.Format(time.RFC3339)
	}
	s.applyBidStatus(ctx, bid, status)
}

func (s *Service) failBid(ctx context.Context, bid *v1alpha1.Bid, reason, message string, now time.Time) {
	if bid.Status.Phase == v1alpha1.BidPhaseFailed {
		return
	}
	message = statusError(errors.New(message))
	status := cloneBidStatus(bid.Status)
	status.Phase = v1alpha1.BidPhaseFailed
	status.Message = message
	status.Timings.FinishedAt = now.Format(time.RFC3339)
	setBidCondition(&status, bid.Generation, v1alpha1.BidConditionFailed, metav1.ConditionTrue, reason, message)
	if s.applyBidStatus(ctx, bid, status) {
		s.log.Info("a bid failed", "bid", bid.Name, "reason", reason, "message", message)
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
	if status.Phase == v1alpha1.BidPhasePublished && status.Bid != nil && status.Bid.URI == ref.URI && status.Bid.CID == ref.CID && status.LastError == "" {
		return false
	}
	status.Phase = v1alpha1.BidPhasePublished
	status.Bid = &v1alpha1.BidRecordRef{URI: ref.URI, CID: ref.CID}
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
	configRef, err := s.ensureRecordAt(ctx, guestidentity.WIFSimpleNSID, rkey+"-config", configRecord)
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
	payload, err := s.ensureRecordAt(ctx, market.BidsFreeNSID, rkey+"-payload", payloadRecord)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid payload: %w", err)
	}
	record, err := market.BuildBid(market.BidOptions{
		RFP:          market.RecordRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID},
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
	ref, err := s.ensureRecordAt(ctx, market.BidNSID, rkey, signed)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid: %w", err)
	}
	s.log.Info("the bidder published a bid", "rfp", bid.Spec.Rfp.URI, "bid", ref.URI)
	return ref, nil
}

func (s *Service) ensureRecordAt(ctx context.Context, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if s.records == nil || s.did == "" {
		return market.StrongRef{}, errors.New("the bidder has no record store or did")
	}
	return ensureStoreRecordAt(ctx, s.records, s.did, collection, rkey, record)
}

func ensureStoreRecordAt(ctx context.Context, records RecordStore, did, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if records == nil || did == "" {
		return market.StrongRef{}, errors.New("the bidder has no record store or did")
	}
	if found, err := records.GetRecord(ctx, did, collection, rkey); err == nil {
		return market.NewStrongRef(found.URI, found.CID), nil
	} else if !errors.Is(err, market.ErrNotFound) {
		return market.StrongRef{}, err
	}
	ref, err := records.CreateRecordAt(ctx, did, collection, rkey, record)
	if err == nil {
		return ref, nil
	}
	if found, getErr := records.GetRecord(ctx, did, collection, rkey); getErr == nil {
		return market.NewStrongRef(found.URI, found.CID), nil
	}
	return market.StrongRef{}, err
}

type createOnlyWriter struct {
	records RecordStore

	did string
}

func (w createOnlyWriter) CreateRecord(ctx context.Context, repo, collection string, record map[string]any) (market.StrongRef, error) {
	return w.records.CreateRecord(ctx, repo, collection, record)
}

func (w createOnlyWriter) PutRecord(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	return ensureStoreRecordAt(ctx, w.records, repo, collection, rkey, record)
}

func (w createOnlyWriter) DeleteRecord(ctx context.Context, repo, collection, rkey string) error {
	return w.records.DeleteRecord(ctx, repo, collection, rkey)
}

func (s *Service) existingBidRecord(ctx context.Context, bid *v1alpha1.Bid) (market.StrongRef, bool, error) {
	if s.records == nil || s.did == "" {
		return market.StrongRef{}, false, nil
	}
	if bid.Status.Bid != nil && bid.Status.Bid.URI != "" {
		rec, err := s.records.Resolve(ctx, market.RecordRef{URI: bid.Status.Bid.URI, CID: bid.Status.Bid.CID})
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
	if s.records == nil || bid.Spec.Rfp.URI == "" {
		return nil
	}
	resolved, err := s.records.Resolve(ctx, market.RecordRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID})
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
		ReceiptURI:   info.receiptURI,
		ReceiptCid:   info.receiptCID,
		AcceptAuthor: info.acceptAuthor,
		Reason:       "deleted_on_request",
		At:           now.Format(time.RFC3339),
	}
	setBidCondition(&status, bid.Generation, BidConditionReleasing, metav1.ConditionTrue, reason, message)
	s.applyBidStatus(ctx, bid, status)
}

func (s *Service) markBidReleaseRequested(ctx context.Context, bid *v1alpha1.Bid, info contractInfo, request, event *v1alpha1.RecordRef, at string) {
	if bid == nil {
		return
	}
	status := cloneBidStatus(bid.Status)
	terminated := &v1alpha1.BidTerminated{
		ReceiptURI:   info.receiptURI,
		ReceiptCid:   info.receiptCID,
		AcceptAuthor: info.acceptAuthor,
		Reason:       "deleted_on_request",
		At:           at,
	}
	if request != nil {
		terminated.PayloadURI = request.URI
		terminated.PayloadCid = request.CID
	}
	if event != nil {
		terminated.EventURI = event.URI
		terminated.EventCid = event.CID
	}
	status.Terminated = terminated
	if info.computeVM != "" {
		setBidCondition(&status, bid.Generation, BidConditionReleasing, metav1.ConditionTrue, "ReleaseRequested",
			"the release of "+info.computeVM+" is requested")
	} else {
		setBidCondition(&status, bid.Generation, BidConditionReleasing, metav1.ConditionTrue, "ReleaseRequested",
			"the release of the accepted contract is requested")
	}
	s.applyBidStatus(ctx, bid, status)
}

func (s *Service) finishRelease(ctx context.Context, bid *v1alpha1.Bid, now time.Time) {
	condition := apimeta.FindStatusCondition(bid.Status.Conditions, BidConditionReleasing)
	if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "Released" {
		return
	}
	terminated := bid.Status.Terminated
	info := contractInfo{
		bidName:      bid.Name,
		computeVM:    bid.Status.ComputeVM,
		receiptURI:   terminated.ReceiptURI,
		receiptCID:   terminated.ReceiptCid,
		acceptAuthor: terminated.AcceptAuthor,
	}
	if bid.Status.Accept != nil {
		info.acceptURI, info.acceptCID = bid.Status.Accept.URI, bid.Status.Accept.CID
	}
	request := &v1alpha1.RecordRef{URI: terminated.PayloadURI, CID: terminated.PayloadCid}
	if request.URI == "" {
		request = nil
	}
	var vm *v1alpha1.ComputeVM
	if info.computeVM != "" && s.vms != nil {
		found, err := s.vms.Get(ctx, info.computeVM)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("reading the ComputeVM of a releasing bid failed", "bid", bid.Name, "err", err)
			}
			return
		}
		vm = found
	}
	if vm != nil {
		observedAt := terminated.At
		if observedAt == "" {
			observedAt = now.Format(time.RFC3339)
		}
		if request == nil {
			request = computevm.ReleaseRequestRef(vm)
		}
		name, err := s.ensureTerminationSync(ctx, info, "deleted_on_request", observedAt, request)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("resuming the termination of a releasing bid failed", "bid", bid.Name, "err", err)
			}
			return
		}
		acked, done := s.terminationAck(ctx, name)
		if !done {
			return
		}
		s.completeBidRelease(ctx, bid, info, acked, now)
		return
	}
	acked, done := s.terminationAckForVM(ctx, info.computeVM)
	if !done {
		acked = nil
	}
	s.completeBidRelease(ctx, bid, info, acked, now)
}

func (s *Service) completeBidRelease(ctx context.Context, bid *v1alpha1.Bid, info contractInfo, acked *v1alpha1.ComputeVMTermination, now time.Time) {
	status := cloneBidStatus(bid.Status)
	status.Timings.FinishedAt = now.Format(time.RFC3339)
	if status.Terminated == nil {
		status.Terminated = &v1alpha1.BidTerminated{
			ReceiptURI:   info.receiptURI,
			ReceiptCid:   info.receiptCID,
			AcceptAuthor: info.acceptAuthor,
			Reason:       "deleted_on_request",
			At:           now.Format(time.RFC3339),
		}
	}
	if acked != nil {
		status.Terminated.PayloadURI = acked.PayloadURI
		status.Terminated.PayloadCid = acked.PayloadCid
		status.Terminated.EventURI = acked.EventURI
		status.Terminated.EventCid = acked.EventCid
	}
	setBidCondition(&status, bid.Generation, BidConditionReleasing, metav1.ConditionFalse, "Released", "the guest is released")
	if s.applyBidStatus(ctx, bid, status) {
		s.log.Info("a released bid is marked terminated", "bid", bid.Name, "computevm", info.computeVM)
	}
	s.contracts.Delete(info.receiptURI)
	s.contracts.Delete(provisionBidHintKey(provisionPayload{Accept: provisionRef{URI: info.acceptURI, CID: info.acceptCID}}))
}

func (s *Service) terminationAck(ctx context.Context, name string) (*v1alpha1.ComputeVMTermination, bool) {
	if name == "" {
		return nil, false
	}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return nil, false
	}
	u, err := dyn.Resource(statesync.StateSyncGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, false
	}
	return terminationOfStateSync(u)
}

func (s *Service) terminationAckForVM(ctx context.Context, vmName string) (*v1alpha1.ComputeVMTermination, bool) {
	if vmName == "" {
		return nil, false
	}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return nil, false
	}
	list, err := dyn.Resource(statesync.StateSyncGVR).List(ctx, metav1.ListOptions{
		LabelSelector: computevm.TerminationSyncVMNameLabel + "=" + vmName,
	})
	if err != nil {
		return nil, false
	}
	for i := range list.Items {
		if acked, ok := terminationOfStateSync(&list.Items[i]); ok {
			return acked, true
		}
	}
	return nil, false
}

func terminationOfStateSync(u *unstructured.Unstructured) (*v1alpha1.ComputeVMTermination, bool) {
	if u == nil {
		return nil, false
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if v1alpha1.StateSyncPhase(phase) != v1alpha1.StateSyncPhaseAcked {
		return nil, false
	}
	payloadURI, _, _ := unstructured.NestedString(u.Object, "status", "acked", "ref")
	if payloadURI == "" {
		return nil, false
	}
	payloadCID, _, _ := unstructured.NestedString(u.Object, "status", "acked", "cid")
	eventURI, _, _ := unstructured.NestedString(u.Object, "status", "acked", "eventUri")
	eventCID, _, _ := unstructured.NestedString(u.Object, "status", "acked", "eventCid")
	at, _, _ := unstructured.NestedString(u.Object, "status", "acked", "at")
	return &v1alpha1.ComputeVMTermination{
		Reason:     "deleted_on_request",
		ObservedAt: at,
		PayloadURI: payloadURI,
		PayloadCid: payloadCID,
		EventURI:   eventURI,
		EventCid:   eventCID,
	}, true
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

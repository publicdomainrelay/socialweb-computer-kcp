package bidder

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

func (b *Bidder) reconcileBid(ctx context.Context, name string) (time.Duration, error) {
	unlock := b.lockBid(name)
	defer unlock()
	bid, ok := b.bids.Get(name)
	if !ok || ctx.Err() != nil {
		return 0, nil
	}
	if wait := b.retryWait(name); wait > 0 {
		return wait, nil
	}
	wait, err := b.stepBid(ctx, &bid, b.now().UTC())
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("reconciling a bid failed; it is retried", "bid", name, "err", err)
		}
		b.retryLater(name)
	}
	return earliest(wait, b.retryWait(name)), nil
}

func (b *Bidder) stepBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) (time.Duration, error) {
	if bid.Status.Terminated != nil && bid.Status.Phase == v1alpha1.BidPhaseWon {
		if err := b.finishRelease(ctx, bid, now); err != nil {
			return 0, err
		}
		return b.finishBid(ctx, bid, now), nil
	}
	switch bid.Status.Phase {
	case v1alpha1.BidPhaseDeciding, v1alpha1.BidPhasePublished:
	default:
		b.bindWonBid(ctx, bid)
		return b.finishBid(ctx, bid, now), nil
	}
	if bidExpired(bid, now) {
		b.expireBid(ctx, bid, now)
		return 0, nil
	}
	var rfp *market.RFP
	if bid.Status.Phase == v1alpha1.BidPhaseDeciding {
		if bid.Spec.PayloadNsid == "" || bid.Spec.Policy.Verdict != "allow" {
			decided, err := b.validateBid(ctx, bid, now)
			if err != nil || decided == nil {
				return 0, err
			}
			if err := b.applyBidSpec(ctx, bid, decided, now); err != nil {
				return 0, err
			}
			rfp = decided
		} else {
			rfp = b.rfpForBid(ctx, bid)
		}
	}
	if bid.Status.Phase == v1alpha1.BidPhasePublished {
		if _, ok := b.verified.Load(bid.Name); ok {
			return b.sendBid(ctx, bid, now), nil
		}
		ref, found, err := b.existingBidRecord(ctx, bid)
		if err != nil {
			return 0, fmt.Errorf("reading the published bid record: %w", err)
		}
		if found {
			b.recordPublishedBid(ctx, bid, ref, nil, now)
			b.verified.Store(bid.Name, true)
			return b.sendBid(ctx, bid, now), nil
		}
	}
	if rfp == nil {
		rfp = b.rfpForBid(ctx, bid)
	}
	if err := b.publishBidRecord(ctx, bid, rfp, now, true); err != nil {
		return 0, fmt.Errorf("resuming the bid publish: %w", err)
	}
	return b.sendBid(ctx, bid, now), nil
}

func (b *Bidder) sendBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) time.Duration {
	wait := untilExpiry(bid, now)
	if bid.Status.Phase != v1alpha1.BidPhasePublished || bid.Status.Bid == nil || bid.Status.Bid.URI == "" {
		return wait
	}
	if bid.Status.Submit == nil {
		rfp := b.rfpForBid(ctx, bid)
		if rfp == nil {
			return earliest(wait, BidSubmitRetry)
		}
		status := cloneBidStatus(bid.Status)
		setSubmitEndpoint(&status, bid.Generation, rfp)
		b.applyBidStatus(ctx, bid, status)
		if bid.Status.Submit == nil {
			return wait
		}
	}
	submit := bid.Status.Submit
	if !submitPending(bid) {
		return wait
	}
	if submitWindowOver(bid, now) {
		status := cloneBidStatus(bid.Status)
		conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionSubmitted, metav1.ConditionFalse, "WindowOver",
			"the bid was not submitted to "+submit.Endpoint+" within "+BidSubmitWindow.String()+" of its publish")
		if b.applyBidStatus(ctx, bid, status) {
			b.log.Warn("the bid is no longer resent to the requester: its submit window is over", "bid", bid.Name, "url", submit.Endpoint,
				"attempts", submit.Attempts, "window", BidSubmitWindow, "err", submit.LastError)
		}
		return wait
	}
	if next := submitBackoff(submit, now); next > 0 {
		return earliest(wait, next)
	}
	ref := market.NewStrongRef(bid.Status.Bid.URI, bid.Status.Bid.CID)
	submitCtx, cancel := context.WithTimeout(ctx, BidSubmitTimeout)
	err := b.submitBid(submitCtx, submit.Endpoint, ref)
	cancel()
	if ctx.Err() != nil {
		return wait
	}
	at := b.now().UTC()
	status := cloneBidStatus(bid.Status)
	next := *submit
	next.Attempts++
	next.LastAttemptAt = at.Format(time.RFC3339)
	status.Submit = &next
	switch {
	case err == nil:
		next.SubmittedAt = at.Format(time.RFC3339)
		next.LastError = ""
		conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionSubmitted, metav1.ConditionTrue, "Submitted",
			"the bid is submitted to "+submit.Endpoint)
		if b.applyBidStatus(ctx, bid, status) {
			b.log.Info("the bid is submitted to the requester", "bid", bid.Name, "url", submit.Endpoint, "attempts", next.Attempts)
		}
		return wait
	case errors.Is(err, errSubmitUnresolvable):
		next.LastError = statusError(err)
		conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionSubmitted, metav1.ConditionFalse, "Unresolvable", next.LastError)
		b.applyBidStatus(ctx, bid, status)
		return wait
	case submitRefused(err):
		b.log.Warn("the requester refused the bid", "bid", bid.Name, "url", submit.Endpoint, "err", err)
		next.LastError = statusError(err)
		conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionSubmitted, metav1.ConditionFalse, "Refused", next.LastError)
		b.applyBidStatus(ctx, bid, status)
		return wait
	}
	next.LastError = statusError(err)
	if next.LastError != submit.LastError {
		b.log.Warn("submitting the bid to the requester failed; it is retried", "bid", bid.Name, "url", submit.Endpoint, "err", err,
			"attempts", next.Attempts, "retryIn", submitDelay(next.Attempts))
	}
	conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionSubmitted, metav1.ConditionFalse, "Retrying", next.LastError)
	b.applyBidStatus(ctx, bid, status)
	return earliest(wait, submitBackoff(&next, at))
}

func submitRefused(err error) bool {
	var callErr *participant.CallError
	return participant.Rejected(err) && errors.As(err, &callErr) && callErr.Status != http.StatusNotFound
}

func setSubmitEndpoint(status *v1alpha1.BidStatus, generation int64, rfp *market.RFP) {
	if status.Submit != nil || rfp == nil {
		return
	}
	endpoint := strings.TrimSpace(rfp.SubmitBid)
	status.Submit = &v1alpha1.BidSubmit{Endpoint: endpoint}
	if endpoint == "" {
		return
	}
	conds.SetStatus(&status.Conditions, generation, v1alpha1.BidConditionSubmitted, metav1.ConditionFalse, "Pending",
		"the bid is to be submitted to "+endpoint)
}

func submitPending(bid *v1alpha1.Bid) bool {
	submit := bid.Status.Submit
	if submit == nil || submit.Endpoint == "" || submit.SubmittedAt != "" {
		return false
	}
	condition := apimeta.FindStatusCondition(bid.Status.Conditions, v1alpha1.BidConditionSubmitted)
	return condition == nil || (condition.Reason != "Refused" && condition.Reason != "Unresolvable" && condition.Reason != "WindowOver")
}

func submitWindowOver(bid *v1alpha1.Bid, now time.Time) bool {
	published, err := time.Parse(time.RFC3339, bid.Status.Timings.PublishedAt)
	return err == nil && !now.Before(published.Add(BidSubmitWindow))
}

func submitDelay(attempts int32) time.Duration {
	return min(BidSubmitRetry<<min(max(attempts-1, 0), 5), BidSubmitRetryCap)
}

func submitBackoff(submit *v1alpha1.BidSubmit, now time.Time) time.Duration {
	if submit.Attempts == 0 {
		return 0
	}
	last, err := time.Parse(time.RFC3339, submit.LastAttemptAt)
	if err != nil {
		return 0
	}
	return max(last.Add(submitDelay(submit.Attempts)).Sub(now), 0)
}

func untilExpiry(bid *v1alpha1.Bid, now time.Time) time.Duration {
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(bid.Spec.ExpiresAt))
	if err != nil {
		return 0
	}
	return max(expires.Sub(now), time.Second)
}

func (b *Bidder) validateBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) (*market.RFP, error) {
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID})
	if err != nil {
		return nil, fmt.Errorf("resolving the RFP of a bid: %w", err)
	}
	rfp, err := market.ParseRFP(resolved.Value)
	if err != nil {
		b.failBid(ctx, bid, "InvalidRFP", err.Error(), now)
		return nil, nil
	}
	if err := b.verifier.VerifyRecord(ctx, resolved); err != nil {
		b.failBid(ctx, bid, "InvalidSignature", err.Error(), now)
		return nil, nil
	}
	if scope := b.policy.Scope(ctx, participant.PolicyInput{SelfDid: b.cfg.DID, CounterpartyDid: bid.Spec.Requester}); !scope.Allow {
		b.failBid(ctx, bid, "ScopeDenied", scope.Reason, now)
		return nil, nil
	}
	decision := b.policy.EvaluatePolicies(ctx, rfp.Policies, b.fullInput(bid.Spec.Requester, bid.Spec.Rfp.URI, bid.Spec.Rfp.CID))
	if !decision.Allow {
		b.failBid(ctx, bid, "PolicyRejected", decision.Reason, now)
		return nil, nil
	}
	return rfp, nil
}

func (b *Bidder) fullInput(requester, rfpURI, rfpCID string) participant.PolicyInput {
	return participant.PolicyInput{
		Perspective:      PerspectiveBidder,
		SelfDid:          b.cfg.DID,
		SubjectDid:       b.cfg.DID,
		CounterpartyDid:  requester,
		RootRequesterDid: requester,
		RFP:              &participant.PolicyRef{URI: rfpURI, CID: rfpCID},
		Mode:             participant.PolicyModeFull,
	}
}

func (b *Bidder) applyBidSpec(ctx context.Context, bid *v1alpha1.Bid, rfp *market.RFP, now time.Time) error {
	attached := make([]commonv1.StrongRef, 0, len(rfp.Policies))
	for _, policy := range rfp.Policies {
		attached = append(attached, commonv1.StrongRef{URI: policy.URI, CID: policy.CID})
	}
	spec := bid.Spec
	spec.PayloadNsid = market.NSIDFromURI(rfp.Payload.URI)
	spec.Policy = v1alpha1.BidPolicy{Attached: attached, Verdict: "allow"}
	if spec.Offer.Kind == "" {
		spec.Offer = v1alpha1.BidOffer{Kind: "free"}
	}
	spec.Provider = cmp.Or(spec.Provider, "kcp")
	spec.ExpiresAt = now.Add(b.cfg.BidTTL).Format(time.RFC3339)
	if err := b.intents.ApplyBidSpec(ctx, bid.Name, spec); err != nil {
		return fmt.Errorf("recording the bid decision: %w", err)
	}
	bid.Spec = spec
	bid.Generation++
	b.bids.Put(bid.Name, *bid)
	status := cloneBidStatus(bid.Status)
	if status.Timings.DecidedAt == "" {
		status.Timings.DecidedAt = now.Format(time.RFC3339)
	}
	b.applyBidStatus(ctx, bid, status)
	return nil
}

func (b *Bidder) failBid(ctx context.Context, bid *v1alpha1.Bid, reason, message string, now time.Time) {
	if bid.Status.Phase == v1alpha1.BidPhaseFailed {
		return
	}
	message = statusError(errors.New(message))
	status := cloneBidStatus(bid.Status)
	status.Phase = v1alpha1.BidPhaseFailed
	status.Message = message
	status.Timings.FinishedAt = now.Format(time.RFC3339)
	conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionFailed, metav1.ConditionTrue, reason, message)
	if b.applyBidStatus(ctx, bid, status) {
		b.log.Info("a bid failed", "bid", bid.Name, "reason", reason, "message", message)
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

func (b *Bidder) expireBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) {
	if bid.Status.Phase != v1alpha1.BidPhaseDeciding && bid.Status.Phase != v1alpha1.BidPhasePublished {
		return
	}
	status := cloneBidStatus(bid.Status)
	status.Phase = v1alpha1.BidPhaseExpired
	status.Timings.FinishedAt = now.Format(time.RFC3339)
	conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionExpired, metav1.ConditionTrue, "Deadline",
		"the bid expired at "+bid.Spec.ExpiresAt)
	b.applyBidStatus(ctx, bid, status)
	b.log.Info("a bid expired", "bid", bid.Name, "expiresAt", bid.Spec.ExpiresAt)
}

func (b *Bidder) publishBidRecord(ctx context.Context, bid *v1alpha1.Bid, rfp *market.RFP, now time.Time, resume bool) error {
	var ref market.StrongRef
	found := false
	if resume {
		existing, ok, err := b.existingBidRecord(ctx, bid)
		if err != nil {
			return err
		}
		ref, found = existing, ok
	}
	if !found {
		published, err := b.writeBidRecord(ctx, bid, now)
		if err != nil {
			b.recordPublishFailure(ctx, bid, err)
			return err
		}
		ref = published
	}
	b.recordPublishedBid(ctx, bid, ref, rfp, now)
	b.verified.Store(bid.Name, true)
	return nil
}

func (b *Bidder) recordPublishFailure(ctx context.Context, bid *v1alpha1.Bid, err error) {
	status := cloneBidStatus(bid.Status)
	status.PublishAttempts++
	status.LastError = statusError(err)
	if status.PublishAttempts >= PublishStallThreshold {
		conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionStalled, metav1.ConditionTrue, "PublishFailed", status.LastError)
	}
	b.applyBidStatus(ctx, bid, status)
}

func (b *Bidder) recordPublishedBid(ctx context.Context, bid *v1alpha1.Bid, ref market.StrongRef, rfp *market.RFP, now time.Time) bool {
	status := cloneBidStatus(bid.Status)
	setSubmitEndpoint(&status, bid.Generation, rfp)
	if status.Phase == v1alpha1.BidPhasePublished && status.Bid != nil && status.Bid.URI == ref.URI && status.Bid.CID == ref.CID && status.LastError == "" {
		return b.applyBidStatus(ctx, bid, status)
	}
	status.Phase = v1alpha1.BidPhasePublished
	status.Bid = &commonv1.StrongRef{URI: ref.URI, CID: ref.CID}
	if status.Timings.PublishedAt == "" {
		status.Timings.PublishedAt = now.Format(time.RFC3339)
	}
	status.LastError = ""
	conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionPublished, metav1.ConditionTrue, "Published", "the bid record is published")
	conds.SetStatus(&status.Conditions, bid.Generation, v1alpha1.BidConditionStalled, metav1.ConditionFalse, "Published", "the bid publish recovered")
	return b.applyBidStatus(ctx, bid, status)
}

func (b *Bidder) writeBidRecord(ctx context.Context, bid *v1alpha1.Bid, now time.Time) (market.StrongRef, error) {
	rkey := bid.Name
	collection, config, err := b.guests.BidConfig(b.cfg.GuestURL, b.cfg.DID, guestseed.AcceptPathVM, now.Format(time.RFC3339))
	if err != nil {
		return market.StrongRef{}, err
	}
	configRecord, err := participant.StructMap(config)
	if err != nil {
		return market.StrongRef{}, err
	}
	configRef, err := b.ensureRecordAt(ctx, collection, rkey+"-config", configRecord)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid config: %w", err)
	}
	payloadRecord, err := market.SignRecord(map[string]any{
		"$type":     market.BidsFreeNSID,
		"reason":    "free",
		"cost":      0,
		"createdAt": now.Format(time.RFC3339),
	}, b.cfg.DID, b.signer)
	if err != nil {
		return market.StrongRef{}, err
	}
	payload, err := b.ensureRecordAt(ctx, market.BidsFreeNSID, rkey+"-payload", payloadRecord)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid payload: %w", err)
	}
	record, err := market.BuildBid(market.BidOptions{
		RFP:          market.RecordRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID},
		Config:       &market.RecordRef{URI: configRef.URI, CID: configRef.CID},
		Payload:      market.RecordRef{URI: payload.URI, CID: payload.CID},
		SubmitAccept: b.cfg.EndpointURL,
		CreatedAt:    now.Format(time.RFC3339),
	})
	if err != nil {
		return market.StrongRef{}, err
	}
	value, err := market.EncodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	signed, err := market.SignRecord(value, b.cfg.DID, b.signer)
	if err != nil {
		return market.StrongRef{}, err
	}
	ref, err := b.ensureRecordAt(ctx, market.BidNSID, rkey, signed)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("writing the bid: %w", err)
	}
	b.log.Info("the bidder published a bid", "rfp", bid.Spec.Rfp.URI, "bid", ref.URI)
	return ref, nil
}

func (b *Bidder) ensureRecordAt(ctx context.Context, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	return participant.EnsureRecordAt(ctx, b.records, b.cfg.DID, collection, rkey, record)
}

func (b *Bidder) existingBidRecord(ctx context.Context, bid *v1alpha1.Bid) (market.StrongRef, bool, error) {
	if bid.Status.Bid != nil && bid.Status.Bid.URI != "" {
		rec, err := b.records.Resolve(ctx, market.RecordRef{URI: bid.Status.Bid.URI, CID: bid.Status.Bid.CID})
		switch {
		case err == nil:
			return market.NewStrongRef(rec.URI, rec.CID), true, nil
		case errors.Is(err, market.ErrNotFound):
		default:
			return market.StrongRef{}, false, err
		}
	}
	rec, err := b.records.GetRecord(ctx, b.cfg.DID, market.BidNSID, bid.Name)
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return market.StrongRef{}, false, nil
		}
		return market.StrongRef{}, false, err
	}
	return market.NewStrongRef(rec.URI, rec.CID), true, nil
}

func (b *Bidder) rfpForBid(ctx context.Context, bid *v1alpha1.Bid) *market.RFP {
	if bid.Spec.Rfp.URI == "" {
		return nil
	}
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: bid.Spec.Rfp.URI, CID: bid.Spec.Rfp.CID})
	if err != nil {
		return nil
	}
	rfp, err := market.ParseRFP(resolved.Value)
	if err != nil {
		return nil
	}
	return rfp
}

func (b *Bidder) applyBidStatus(ctx context.Context, bid *v1alpha1.Bid, status v1alpha1.BidStatus) bool {
	if reflect.DeepEqual(status, bid.Status) {
		return false
	}
	if err := b.intents.ApplyBidStatus(ctx, bid.Name, status); err != nil {
		if ctx.Err() == nil {
			b.log.Warn("patching the bid status failed", "bid", bid.Name, "err", err)
		}
		b.retryLater(bid.Name)
		return false
	}
	bid.Status = status
	b.bids.Put(bid.Name, *bid)
	return true
}

func (b *Bidder) markReleaseRequested(ctx context.Context, name string, info contractInfo, request, event *commonv1.StrongRef, at string) {
	if name == "" {
		return
	}
	unlock := b.lockBid(name)
	defer unlock()
	bid, ok := b.bids.Get(name)
	if !ok {
		return
	}
	status := cloneBidStatus(bid.Status)
	terminated := &v1alpha1.BidTerminated{
		ReceiptURI:   info.receiptURI,
		ReceiptCid:   info.receiptCID,
		AcceptAuthor: info.acceptAuthor,
		Reason:       releaseReason,
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
	message := "the release of the accepted contract is requested"
	if info.computeVM != "" {
		message = "the release of " + info.computeVM + " is requested"
	}
	conds.SetStatus(&status.Conditions, bid.Generation, BidConditionReleasing, metav1.ConditionTrue, "ReleaseRequested", message)
	b.applyBidStatus(ctx, &bid, status)
}

func (b *Bidder) finishRelease(ctx context.Context, bid *v1alpha1.Bid, now time.Time) error {
	condition := apimeta.FindStatusCondition(bid.Status.Conditions, BidConditionReleasing)
	if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "Released" {
		return nil
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
	request := &commonv1.StrongRef{URI: terminated.PayloadURI, CID: terminated.PayloadCid}
	if request.URI == "" {
		request = nil
	}
	vm, live := b.vms.Get(info.computeVM)
	if info.computeVM != "" && live {
		observedAt := cmp.Or(terminated.At, now.Format(time.RFC3339))
		if request == nil {
			request = b.guests.ReleaseRequestRef(&vm)
		}
		name, err := b.ensureTerminationSync(ctx, info, observedAt, request)
		if err != nil {
			return fmt.Errorf("resuming the termination of a releasing bid: %w", err)
		}
		sync, ok := b.syncs.Get(name)
		if !ok {
			return nil
		}
		acked, done := terminationOf(&sync)
		if !done {
			return nil
		}
		b.completeBidRelease(ctx, bid, info, acked, now)
		return nil
	}
	b.completeBidRelease(ctx, bid, info, b.terminationForVM(info.computeVM), now)
	return nil
}

func (b *Bidder) terminationForVM(vmName string) *v1alpha1.ComputeVMTermination {
	if vmName == "" {
		return nil
	}
	for _, sync := range b.syncs.List() {
		if b.guests.SyncVM(&sync) != vmName {
			continue
		}
		if acked, ok := terminationOf(&sync); ok {
			return acked
		}
	}
	return nil
}

func terminationOf(sync *v1alpha1.StateSync) (*v1alpha1.ComputeVMTermination, bool) {
	acked := sync.Status.Acked
	if sync.Status.Phase != v1alpha1.StateSyncPhaseAcked || acked == nil || acked.Ref == "" {
		return nil, false
	}
	return &v1alpha1.ComputeVMTermination{
		Reason:     releaseReason,
		ObservedAt: acked.At,
		PayloadURI: acked.Ref,
		PayloadCid: acked.Cid,
		EventURI:   acked.EventURI,
		EventCid:   acked.EventCid,
	}, true
}

func (b *Bidder) completeBidRelease(ctx context.Context, bid *v1alpha1.Bid, info contractInfo, acked *v1alpha1.ComputeVMTermination, now time.Time) {
	status := cloneBidStatus(bid.Status)
	status.Timings.FinishedAt = now.Format(time.RFC3339)
	if status.Terminated == nil {
		status.Terminated = &v1alpha1.BidTerminated{
			ReceiptURI:   info.receiptURI,
			ReceiptCid:   info.receiptCID,
			AcceptAuthor: info.acceptAuthor,
			Reason:       releaseReason,
			At:           now.Format(time.RFC3339),
		}
	}
	if acked != nil {
		terminated := *status.Terminated
		terminated.PayloadURI = acked.PayloadURI
		terminated.PayloadCid = acked.PayloadCid
		terminated.EventURI = acked.EventURI
		terminated.EventCid = acked.EventCid
		status.Terminated = &terminated
	}
	conds.SetStatus(&status.Conditions, bid.Generation, BidConditionReleasing, metav1.ConditionFalse, "Released", "the guest is released")
	if b.applyBidStatus(ctx, bid, status) {
		b.log.Info("a released bid is marked terminated", "bid", bid.Name, "computevm", info.computeVM)
	}
}

func bidFinished(bid *v1alpha1.Bid) bool {
	for _, t := range []string{v1alpha1.BidConditionComplete, v1alpha1.BidConditionFailed} {
		if c := apimeta.FindStatusCondition(bid.Status.Conditions, t); c != nil && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func bidFinish(bid *v1alpha1.Bid, vmLive func(string) bool, now time.Time) (string, time.Time, string, time.Duration) {
	finished := func() time.Time {
		if at, err := time.Parse(time.RFC3339, bid.Status.Timings.FinishedAt); err == nil {
			return at
		}
		return bid.CreationTimestamp.Time
	}
	switch bid.Status.Phase {
	case v1alpha1.BidPhaseLost, v1alpha1.BidPhaseExpired:
		return v1alpha1.BidConditionComplete, finished(), string(bid.Status.Phase), 0
	case v1alpha1.BidPhaseFailed:
		return v1alpha1.BidConditionFailed, finished(), string(bid.Status.Phase), 0
	case v1alpha1.BidPhaseWon:
		if vm := bid.Status.ComputeVM; vm != "" {
			if vmLive(vm) {
				return "", time.Time{}, "", 0
			}
			return v1alpha1.BidConditionComplete, now, "ComputeVMGone", 0
		}
		at, err := time.Parse(time.RFC3339, bid.Status.Timings.AcceptedAt)
		if err != nil {
			return "", time.Time{}, "", 0
		}
		if wait := at.Add(WonUnboundWait).Sub(now); wait > 0 {
			return "", time.Time{}, "", wait
		}
		return v1alpha1.BidConditionComplete, now, "NeverBound", 0
	}
	return "", time.Time{}, "", 0
}

func (b *Bidder) bindWonBid(ctx context.Context, bid *v1alpha1.Bid) {
	if bid.Status.Phase != v1alpha1.BidPhaseWon || bid.Status.ComputeVM != "" || bid.Status.Accept == nil {
		return
	}
	vm := b.vmForAccept(bid.Status.Accept.URI, bid.Status.Accept.CID)
	if vm == nil {
		return
	}
	status := cloneBidStatus(bid.Status)
	status.ComputeVM = vm.Name
	if b.applyBidStatus(ctx, bid, status) {
		b.log.Info("a Won Bid is bound to the ComputeVM that serves its accept", "bid", bid.Name, "computevm", vm.Name)
	}
}

func (b *Bidder) finishBid(ctx context.Context, bid *v1alpha1.Bid, now time.Time) time.Duration {
	if bidFinished(bid) {
		return 0
	}
	live := func(name string) bool {
		_, ok := b.vms.Get(name)
		return ok
	}
	kind, at, reason, wait := bidFinish(bid, live, now)
	if kind == "" {
		return wait
	}
	status := cloneBidStatus(bid.Status)
	conds.SetStatusAt(&status.Conditions, bid.Generation, kind, metav1.ConditionTrue, reason, "the Bid is finished; its TTL runs from now", at)
	if b.applyBidStatus(ctx, bid, status) {
		b.log.Info("a finished Bid carries its condition; the TTL controller removes it after its TTL", "bid", bid.Name, "ttl", BidTTL)
	}
	return 0
}

func bidOwner(bid *v1alpha1.Bid) *metav1.OwnerReference {
	if bid == nil || bid.UID == "" {
		return nil
	}
	return &metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Bid", Name: bid.Name, UID: bid.UID}
}

func cloneBidStatus(status v1alpha1.BidStatus) v1alpha1.BidStatus {
	if status.Conditions != nil {
		status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	}
	return status
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

func bidName(did, rfpURI, rfpCID string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + rfpURI + "#" + rfpCID))
	return "bid-" + hex.EncodeToString(sum[:])[:20]
}

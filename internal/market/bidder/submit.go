package bidder

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const SubmitEventLookupTimeout = 3 * time.Second

type contractInfo struct {
	bidName string

	computeVM string

	receiptURI string

	receiptCID string

	acceptURI string

	acceptCID string

	acceptAuthor string

	submitEventURL string
}

func (b *Bidder) SubmitRFP(ctx context.Context, rfpURI, rfpCID, issuer string) (string, error) {
	if b.cfg.Suspend {
		return "", ErrSuspended
	}
	bid, err := b.enqueueBid(ctx, rfpURI, rfpCID, issuer)
	if err != nil {
		return "", err
	}
	return bid.Name, nil
}

func (b *Bidder) SubmitAccept(ctx context.Context, acceptURI, acceptCID, issuer string) (string, error) {
	payload := provisionPayload{RequesterDID: issuer}
	payload.Accept.URI, payload.Accept.CID = acceptURI, acceptCID
	return b.ensureProvisionSync(ctx, payload)
}

func (b *Bidder) SubmitEvent(ctx context.Context, uri, cid string, record map[string]any, issuer string) error {
	return b.applyMarketEvent(ctx, uri, cid, record, issuer)
}

func (b *Bidder) enqueueBid(ctx context.Context, rfpURI, rfpCID, issuer string) (v1alpha1.Bid, error) {
	now := b.now().UTC()
	name := bidName(b.cfg.DID, rfpURI, rfpCID)
	unlock := b.lockBid(name)
	defer unlock()
	if existing, ok := b.bids.Get(name); ok {
		return b.claimBid(ctx, existing, now)
	}
	bid := v1alpha1.Bid{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.BidSpec{
			Rfp:                     commonv1.StrongRef{URI: rfpURI, CID: rfpCID},
			Requester:               issuer,
			ExpiresAt:               now.Add(b.cfg.BidTTL).Format(time.RFC3339),
			TTLSecondsAfterFinished: ptr.To(BidTTLSeconds),
		},
	}
	err := b.intents.CreateBid(ctx, bid)
	if errors.Is(err, contract.ErrExists) {
		waitCtx, cancel := context.WithTimeout(ctx, AwaitTimeout)
		defer cancel()
		var existing v1alpha1.Bid
		found := false
		_ = b.bids.Await(waitCtx, func() bool {
			existing, found = b.bids.Get(name)
			return found
		})
		if !found {
			return bid, nil
		}
		return b.claimBid(ctx, existing, now)
	}
	if err != nil {
		return v1alpha1.Bid{}, err
	}
	status := decidingBidStatus(now, b.cfg.ClaimedBy)
	if err := b.intents.ApplyBidStatus(ctx, name, status); err != nil {
		return v1alpha1.Bid{}, err
	}
	bid.Status = status
	return bid, nil
}

func (b *Bidder) claimBid(ctx context.Context, bid v1alpha1.Bid, now time.Time) (v1alpha1.Bid, error) {
	if bid.Status.Phase != "" {
		return bid, nil
	}
	status := decidingBidStatus(now, b.cfg.ClaimedBy)
	if err := b.intents.ApplyBidStatus(ctx, bid.Name, status); err != nil {
		return v1alpha1.Bid{}, err
	}
	bid.Status = status
	b.bids.Put(bid.Name, bid)
	return bid, nil
}

func decidingBidStatus(now time.Time, claimedBy string) v1alpha1.BidStatus {
	return v1alpha1.BidStatus{
		Phase:     v1alpha1.BidPhaseDeciding,
		ClaimedBy: claimedBy,
		Timings:   v1alpha1.BidTimings{RfpSeenAt: now.Format(time.RFC3339)},
	}
}

func (b *Bidder) applyMarketEvent(ctx context.Context, uri, cid string, record map[string]any, issuer string) error {
	event, err := market.ParseEvent(record)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEventInvalid, err)
	}
	if market.NSIDFromURI(event.Payload.URI) != market.ComputeEventsVMDeleteNSID || market.ATURIAuthority(event.Receipt.URI) != b.cfg.DID {
		return nil
	}
	if b.releasedByReceipt(event.Receipt.URI) {
		return nil
	}
	info, vm, found := b.localContractForReceipt(event.Receipt.URI)
	if found && info.acceptAuthor != "" && info.acceptAuthor != issuer {
		return ErrEventForbidden
	}
	if found && vm.UID != "" {
		request := &commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID}
		if err := b.ensureInboundTerminationSync(ctx, vm, request, issuer, uri, cid, record); err != nil {
			return fmt.Errorf("the termination intent could not be recorded: %w", err)
		}
		return nil
	}
	if err := b.ensureReleaseSync(ctx, event.Receipt.URI, issuer, uri, cid, record); err != nil {
		return fmt.Errorf("the release intent could not be recorded: %w", err)
	}
	return nil
}

type unsentBid struct {
	endpoint string

	ref market.StrongRef
}

func (b *Bidder) submitBid(ctx context.Context, endpoint string, ref market.StrongRef) error {
	raw := strings.TrimSpace(endpoint)
	endpoint = b.submitEventURL(ctx, raw)
	if endpoint == "" {
		b.log.Warn("submitting the bid failed: the endpoint does not resolve", "url", raw, "bid", ref.URI)
		return nil
	}
	record, err := b.records.GetRecord(ctx, market.ATURIAuthority(ref.URI), market.NSIDFromURI(ref.URI), participant.LastSegment(ref.URI))
	if err != nil {
		return fmt.Errorf("reading the bid record %s: %w", ref.URI, err)
	}
	body := map[string]any{"uri": ref.URI, "cid": ref.CID, "record": record.Value}
	return b.outbound.Post(ctx, endpoint, market.SubmitBidNSID, body)
}

func (b *Bidder) submitEventURL(ctx context.Context, ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	did, fragment, found := strings.Cut(ref, "#")
	if !found || strings.TrimSpace(fragment) == "" || b.directory == nil {
		return ""
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, SubmitEventLookupTimeout)
	defer cancel()
	ident, err := b.directory.LookupDID(lookupCtx, parsed)
	if err != nil || ident == nil {
		return ""
	}
	service, ok := ident.Services[fragment]
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(service.URL), "/")
}

func (b *Bidder) createReceipt(ctx context.Context, refs market.ReceiptRefs, accept market.ResolvedRecord, issuer string) (string, string, error) {
	value := make(map[string]any, len(accept.Value))
	maps.Copy(value, accept.Value)
	rkey := market.ReceiptRKey(refs.Accept.URI, refs.Accept.CID)
	existing, err := b.records.GetRecord(ctx, b.cfg.DID, market.ReceiptNSID, rkey)
	if err == nil {
		return existing.URI, existing.CID, nil
	}
	if !errors.Is(err, market.ErrNotFound) {
		return "", "", err
	}
	receipt, err := market.CreateReceiptRecordAt(ctx, participant.CreateOnly{Records: b.records}, b.cfg.DID, rkey, refs, market.ReceiptSubject{
		AcceptRecord:        value,
		AcceptRepositoryDID: issuer,
	}, b.signer)
	if err != nil {
		return "", "", err
	}
	return receipt.URI, receipt.CID, nil
}

func (b *Bidder) bidForAccept(accept *market.Accept) (v1alpha1.Bid, error) {
	if market.ATURIAuthority(accept.Bid.URI) != b.cfg.DID {
		return v1alpha1.Bid{}, fmt.Errorf("the accepted bid %s is not this bidder's", accept.Bid.URI)
	}
	rkey := participant.LastSegment(accept.Bid.URI)
	bid, ok := b.bids.Find(func(bid v1alpha1.Bid) bool {
		if bid.Spec.Rfp.URI != accept.RFP.URI || bid.Name != rkey {
			return false
		}
		return bid.Status.Bid == nil || bid.Status.Bid.URI == "" || bid.Status.Bid.URI == accept.Bid.URI
	})
	if !ok {
		return v1alpha1.Bid{}, fmt.Errorf("no bid of this bidder matches %s", accept.Bid.URI)
	}
	return bid, nil
}

func (b *Bidder) localContractForReceipt(receiptURI string) (contractInfo, *v1alpha1.ComputeVM, bool) {
	vm, ok := b.vms.Find(func(vm v1alpha1.ComputeVM) bool {
		return vm.Spec.Contract != nil && vm.Spec.Contract.Receipt.URI == receiptURI
	})
	if !ok {
		return contractInfo{}, nil, false
	}
	terms := vm.Spec.Contract
	info := contractInfo{computeVM: vm.Name, receiptURI: terms.Receipt.URI, receiptCID: terms.Receipt.CID,
		acceptURI: terms.Accept.URI, acceptCID: terms.Accept.CID, acceptAuthor: vm.Spec.Requester.Did,
		submitEventURL: terms.SubmitEventURL}
	if bid := b.bidForVM(&vm); bid != nil {
		info.bidName = bid.Name
	}
	return info, &vm, true
}

func (b *Bidder) releasedByReceipt(receiptURI string) bool {
	_, ok := b.bids.Find(func(bid v1alpha1.Bid) bool {
		terminated := bid.Status.Terminated
		if terminated == nil || terminated.ReceiptURI != receiptURI {
			return false
		}
		condition := apimeta.FindStatusCondition(bid.Status.Conditions, BidConditionReleasing)
		return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "Released"
	})
	return ok
}

func (b *Bidder) vmForAccept(acceptURI, acceptCID string) *v1alpha1.ComputeVM {
	key := b.guests.AcceptKey(acceptURI, acceptCID)
	if key == "" {
		return nil
	}
	vm, ok := b.vms.Find(func(vm v1alpha1.ComputeVM) bool {
		return b.guests.AcceptKeyOf(&vm) == key && vm.DeletionTimestamp == nil && !b.guests.Ended(&vm)
	})
	if !ok {
		return nil
	}
	return &vm
}

func (b *Bidder) bidForVM(vm *v1alpha1.ComputeVM) *v1alpha1.Bid {
	if vm == nil {
		return nil
	}
	if bid, ok := b.bids.Find(func(bid v1alpha1.Bid) bool { return vm.Name != "" && bid.Status.ComputeVM == vm.Name }); ok {
		return &bid
	}
	if terms := vm.Spec.Contract; terms != nil {
		return b.bidForAcceptStatus(terms.Accept.URI, terms.Accept.CID)
	}
	return nil
}

func (b *Bidder) bidForAcceptStatus(acceptURI, acceptCID string) *v1alpha1.Bid {
	bid, ok := b.bids.Find(func(bid v1alpha1.Bid) bool {
		accept := bid.Status.Accept
		return accept != nil && accept.URI == acceptURI && accept.CID == acceptCID
	})
	if !ok {
		return nil
	}
	return &bid
}

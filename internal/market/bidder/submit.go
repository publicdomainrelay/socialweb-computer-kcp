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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
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
	if b.rfpClosed(rfpURI, nil, "submitRfp") {
		return "", ErrRFPClosed
	}
	kind, err := b.rfpKind(ctx, rfpURI, rfpCID)
	if err != nil {
		return "", err
	}
	if !b.servesKind(kind) {
		b.log.Info("bidder: an RFP asks for a compute kind this bidder does not serve", "rfp", rfpURI, "kind", kind)
		return "", ErrKindUnsupported
	}
	bid, err := b.enqueueBid(ctx, rfpURI, rfpCID, issuer)
	if err != nil {
		return "", err
	}
	return bid.Name, nil
}

func (b *Bidder) AnswerIngress(ctx context.Context, offer tunnel.Offer, requesterDID string) (tunnel.Answer, error) {
	if b.ingress == nil {
		return tunnel.Answer{}, errors.New("bidder: no WebRTC ingress transport is configured")
	}
	if offer.Peer != "" && offer.Peer != b.cfg.DID {
		return tunnel.Answer{}, fmt.Errorf("bidder: the ingress offer names %s and this bidder is %s", offer.Peer, b.cfg.DID)
	}
	id := tunnel.ID(requesterDID)
	if id == "" || offer.ID != id {
		return tunnel.Answer{}, fmt.Errorf("bidder: the ingress offer names %s and the requester %s", offer.ID, requesterDID)
	}
	answer, err := b.ingress.Answer(ctx, offer)
	if err != nil {
		return tunnel.Answer{}, err
	}
	return answer, nil
}

func (b *Bidder) AnswerIngressRecords(ctx context.Context, refs []market.StrongRef, requesterDID string) ([]market.StrongRef, error) {
	if b.ingress == nil {
		return nil, errors.New("bidder: no WebRTC ingress transport is configured")
	}
	for _, ref := range refs {
		if market.ATURIAuthority(ref.URI) != requesterDID || market.NSIDFromURI(ref.URI) != market.IngressWebRTCNSID {
			continue
		}
		resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: ref.URI, CID: ref.CID})
		if err != nil {
			b.log.Warn("bidder_webrtc_offer_unresolved", "offer", ref.URI, "err", err)
			continue
		}
		record, err := market.ParseIngressWebRTC(resolved.Value)
		if err != nil {
			b.log.Warn("bidder_webrtc_offer_invalid", "offer", ref.URI, "err", err)
			continue
		}
		if err := record.Validate(); err != nil {
			b.log.Warn("bidder_webrtc_offer_invalid", "offer", ref.URI, "err", err)
			continue
		}
		if record.Mode != market.IngressModeOffer {
			continue
		}
		if record.Peer != b.cfg.DID || record.ID != tunnel.ID(requesterDID) {
			b.log.Warn("bidder_webrtc_offer_mismatch", "offer", ref.URI, "requester", requesterDID)
			continue
		}
		answer, err := b.AnswerIngress(ctx, tunnel.Offer{
			Kind: tunnel.KindWebRTC, ID: record.ID, Session: record.Session,
			Peer: record.Peer, Type: tunnel.SignalOffer, SDP: record.Sdp,
		}, requesterDID)
		if err != nil {
			return nil, err
		}
		value, err := market.EncodeRecord(market.IngressWebRTC{
			Kind: tunnel.KindWebRTC, Mode: market.IngressModeAnswer, ID: answer.ID, Peer: requesterDID,
			Session: answer.Session, Sdp: answer.SDP, Descriptor: record.Descriptor,
			StunUrls: []string{tunnel.DefaultSTUN}, CreatedAt: market.NowISO(),
		})
		if err != nil {
			return nil, err
		}
		answerRef, err := b.records.CreateRecord(ctx, b.cfg.DID, market.IngressWebRTCNSID, value)
		if err != nil {
			return nil, err
		}
		b.ingressMu.Lock()
		b.ingressLive[answer.ID] = true
		b.ingressMu.Unlock()
		out := []market.StrongRef{answerRef}
		if descriptor := b.ingressDescriptor(ctx); descriptor != nil {
			out = append(out, market.NewStrongRef(descriptor.URI, descriptor.CID))
		}
		return out, nil
	}
	return nil, errors.New("bidder: the request names no ingress offer for this bidder")
}

func (b *Bidder) ingressDescriptor(ctx context.Context) *market.RecordRef {
	if b.ingress == nil || b.records == nil {
		return nil
	}
	id := tunnel.ID(b.cfg.DID)
	if id == "" {
		return nil
	}
	b.ingressRefMu.Lock()
	cached := b.ingressRef
	b.ingressRefMu.Unlock()
	if cached != nil {
		return &market.RecordRef{URI: cached.URI, CID: cached.CID}
	}
	value, err := market.EncodeRecord(market.IngressWebRTC{
		Kind: tunnel.KindWebRTC, Mode: market.IngressModeListen, ID: id,
		StunUrls: []string{tunnel.DefaultSTUN}, CreatedAt: market.NowISO(),
	})
	if err != nil {
		b.log.Warn("bidder_webrtc_descriptor_failed", "err", err)
		return nil
	}
	ref, err := participant.EnsureRecordAt(ctx, b.records, b.cfg.DID, market.IngressWebRTCNSID, "webrtc", value)
	if err != nil {
		b.log.Warn("bidder_webrtc_descriptor_failed", "err", err)
		return nil
	}
	b.ingressRefMu.Lock()
	b.ingressRef = &ref
	b.ingressRefMu.Unlock()
	b.log.Info("bidder_webrtc_descriptor", "uri", ref.URI, "cid", ref.CID)
	return &market.RecordRef{URI: ref.URI, CID: ref.CID}
}

func (b *Bidder) ingressRecords(ctx context.Context) []market.RecordRef {
	ref := b.ingressDescriptor(ctx)
	if ref == nil {
		return nil
	}
	return []market.RecordRef{*ref}
}

func (b *Bidder) IngressRecords(ctx context.Context) []market.StrongRef {
	refs := b.ingressRecords(ctx)
	out := make([]market.StrongRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, market.NewStrongRef(ref.URI, ref.CID))
	}
	return out
}

func (b *Bidder) liveIngress(ownerDID string) string {
	id := tunnel.ID(ownerDID)
	if id == "" {
		return ""
	}
	b.ingressMu.Lock()
	live := b.ingressLive[id]
	b.ingressMu.Unlock()
	if !live {
		return ""
	}
	return id
}

func (b *Bidder) submitEndpoint(rfp *market.RFP, requesterDID string) string {
	if rfp == nil {
		return ""
	}
	if id := b.liveIngress(requesterDID); id != "" {
		return tunnel.Ref(id)
	}
	return strings.TrimSpace(rfp.SubmitBid)
}

func (b *Bidder) eventEndpoint(ctx context.Context, requesterDID, serviceRef string) string {
	if id := b.liveIngress(requesterDID); id != "" {
		return tunnel.Ref(id)
	}
	return b.submitEventURL(ctx, serviceRef)
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
	published, err := b.publishedBid(ctx, name)
	if err != nil {
		return v1alpha1.Bid{}, fmt.Errorf("reading the bidder's bid record for the RFP: %w", err)
	}
	if published != nil {
		b.log.Info("bidder_rfp_already_bid", "rfp", rfpURI, "bid", published.URI)
		return v1alpha1.Bid{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1alpha1.BidStatus{Phase: v1alpha1.BidPhasePublished,
			Bid: &commonv1.StrongRef{URI: published.URI, CID: published.CID}}}, nil
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
	err = b.intents.CreateBid(ctx, bid)
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

func (b *Bidder) publishedBid(ctx context.Context, name string) (*market.ResolvedRecord, error) {
	record, err := b.records.GetRecord(ctx, b.cfg.DID, market.BidNSID, name)
	switch {
	case err == nil:
		return &record, nil
	case errors.Is(err, market.ErrNotFound):
		return nil, nil
	}
	return nil, err
}

func (b *Bidder) rfpClosed(rfpURI string, record map[string]any, via string) bool {
	if !market.RFPBidWindowOver(rfpURI, record, b.now()) {
		return false
	}
	if n := b.closedRFPs.Add(1); n == 1 || n%closedRFPLogEvery == 0 {
		b.log.Info("bidder_rfp_closed", "rfp", rfpURI, "via", via, "closed", n, "bidWindow", market.RFPBidWindow)
	}
	return true
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
	if market.IsGuestEventNSID(market.NSIDFromURI(event.Payload.URI)) && market.ATURIAuthority(event.Receipt.URI) == b.cfg.DID {
		return b.applyGuestEvent(ctx, uri, cid, record, event, issuer)
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

var errSubmitUnresolvable = errors.New("bidder: the submit endpoint does not resolve")

func (b *Bidder) submitBid(ctx context.Context, endpoint string, ref market.StrongRef) error {
	raw := strings.TrimSpace(endpoint)
	endpoint = b.submitEventURL(ctx, raw)
	if endpoint == "" {
		b.log.Warn("submitting the bid failed: the endpoint does not resolve", "url", raw, "bid", ref.URI)
		return fmt.Errorf("%w: %s", errSubmitUnresolvable, raw)
	}
	record, err := b.records.GetRecord(ctx, market.ATURIAuthority(ref.URI), market.NSIDFromURI(ref.URI), participant.LastSegment(ref.URI))
	if err != nil {
		return fmt.Errorf("reading the bid record %s: %w", ref.URI, err)
	}
	body := map[string]any{"uri": ref.URI, "cid": ref.CID, "record": record.Value}
	if refs := b.IngressRecords(ctx); len(refs) > 0 {
		body["ingress"] = refs
	}
	return b.outbound.Post(ctx, endpoint, market.SubmitBidNSID, body)
}

func (b *Bidder) submitEventURL(ctx context.Context, ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	if tunnel.IsRef(ref) {
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

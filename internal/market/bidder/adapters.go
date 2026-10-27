package bidder

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

type outboxAdapter struct {
	kind string

	observe func(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error)

	apply func(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error)

	remove func(ctx context.Context, request participant.OutboxRequest) error
}

var _ participant.Adapter = (*outboxAdapter)(nil)

func (a *outboxAdapter) Name() string { return a.kind }

func (a *outboxAdapter) Observe(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	return a.observe(ctx, request)
}

func (a *outboxAdapter) Apply(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	return a.apply(ctx, request)
}

func (a *outboxAdapter) Delete(ctx context.Context, request participant.OutboxRequest) error {
	if a.remove == nil {
		return participant.ErrRemoteNotFound
	}
	return a.remove(ctx, request)
}

func (b *Bidder) eventAdapter() participant.Adapter {
	return &outboxAdapter{kind: b.guests.EventAdapter(), observe: b.observeEvent, apply: b.applyEvent, remove: b.deleteEvent}
}

func (b *Bidder) provisionAdapter() participant.Adapter {
	return &outboxAdapter{kind: ProvisionSyncAdapter, observe: b.observeProvision, apply: b.applyProvision}
}

func (b *Bidder) releaseAdapter() participant.Adapter {
	return &outboxAdapter{kind: ReleaseSyncAdapter, observe: b.observeRelease, apply: b.applyRelease}
}

func (b *Bidder) inboxAdapter() participant.Adapter {
	return &outboxAdapter{kind: MarketInboxAdapter, observe: b.observeInbox, apply: b.applyInbox}
}

func payloadHash(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (b *Bidder) stamp() string {
	return b.now().UTC().Format(time.RFC3339)
}

type marketEventPayload struct {
	Collection string `json:"collection"`

	Receipt struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	} `json:"receipt"`

	Record map[string]any `json:"record"`

	SubmitEventURL string `json:"submitEventUrl"`

	PayloadRKey string `json:"payloadRKey"`

	EventRKey string `json:"eventRKey"`

	RequestEvent *marketEventRequest `json:"requestEvent,omitempty"`

	RequesterDID string `json:"requesterDid,omitempty"`
}

func decodeMarketEventPayload(raw json.RawMessage) (marketEventPayload, map[string]any, error) {
	var payload marketEventPayload
	if len(raw) == 0 {
		return payload, nil, errors.New("bidder: the market event payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, nil, fmt.Errorf("bidder: the market event payload is not JSON: %w", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return payload, nil, fmt.Errorf("bidder: the market event payload is not JSON: %w", err)
	}
	switch {
	case strings.TrimSpace(payload.Collection) == "":
		return payload, nil, errors.New("bidder: the market event payload names no collection")
	case payload.Receipt.URI == "" || payload.Receipt.CID == "":
		return payload, nil, errors.New("bidder: the market event payload names no receipt")
	case payload.Record == nil:
		return payload, nil, errors.New("bidder: the market event payload names no record")
	}
	return payload, envelope, nil
}

func (p marketEventPayload) recordKeys(key string) (string, string) {
	prefix := marketEventRKeyPrefix(key)
	payloadRKey := strings.TrimSpace(p.PayloadRKey)
	if payloadRKey == "" {
		sum := sha256.Sum256([]byte(key))
		payloadRKey = prefix + "-payload-" + hex.EncodeToString(sum[:16])
	}
	eventRKey := strings.TrimSpace(p.EventRKey)
	if eventRKey == "" {
		sum := sha256.Sum256([]byte("event:" + key))
		eventRKey = prefix + "-event-" + hex.EncodeToString(sum[:16])
	}
	return payloadRKey, eventRKey
}

func marketEventRKeyPrefix(key string) string {
	prefix := strings.TrimSpace(key)
	if i := strings.IndexByte(prefix, ':'); i >= 0 {
		prefix = prefix[:i]
	}
	var clean strings.Builder
	for _, r := range strings.ToLower(prefix) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			clean.WriteRune(r)
		}
	}
	switch prefix = clean.String(); prefix {
	case "":
		return "market"
	case "terminated":
		return "termination"
	}
	return prefix
}

func unsignedRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if key != "signatures" {
			out[key] = value
		}
	}
	return out
}

func marketEventHash(envelope map[string]any, record map[string]any) (string, error) {
	unsigned := make(map[string]any, len(envelope))
	maps.Copy(unsigned, envelope)
	unsigned["record"] = record
	raw, err := json.Marshal(unsigned)
	if err != nil {
		return "", err
	}
	return payloadHash(raw), nil
}

func recordObservedAt(record map[string]any) string {
	for _, key := range []string{"observedAt", "createdAt"} {
		if value, ok := record[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func (b *Bidder) observeEvent(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, envelope, err := decodeMarketEventPayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	payloadRec, err := b.records.GetRecord(ctx, b.cfg.DID, payload.Collection, payloadRKey)
	if errors.Is(err, market.ErrNotFound) {
		return participant.Observation{}, nil
	}
	if err != nil {
		return participant.Observation{}, err
	}
	eventRec, err := b.records.GetRecord(ctx, b.cfg.DID, market.EventNSID, eventRKey)
	if errors.Is(err, market.ErrNotFound) {
		return participant.Observation{}, nil
	}
	if err != nil {
		return participant.Observation{}, err
	}
	hash, err := marketEventHash(envelope, unsignedRecord(payloadRec.Value))
	if err != nil {
		return participant.Observation{}, err
	}
	return participant.Observation{
		Exists: payload.submitTarget(request) == "" || b.eventDelivered(request),
		Ref:    payloadRec.URI,
		CID:    payloadRec.CID,
		Hash:   hash,
		At:     recordObservedAt(payloadRec.Value),
		Meta: map[string]string{
			"payloadUri": payloadRec.URI,
			"payloadCid": payloadRec.CID,
			"eventUri":   eventRec.URI,
			"eventCid":   eventRec.CID,
		},
	}, nil
}

func (p marketEventPayload) submitTarget(request participant.OutboxRequest) string {
	return cmp.Or(strings.TrimSpace(p.SubmitEventURL), strings.TrimSpace(request.Sink.SubmitEventURL))
}

func deliveredKey(uid string, payload json.RawMessage) string {
	return uid + "#" + payloadHash(payload)
}

func (b *Bidder) eventDelivered(request participant.OutboxRequest) bool {
	_, ok := b.delivered.Load(deliveredKey(request.UID, request.Payload))
	return ok
}

func (b *Bidder) applyEvent(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, _, err := decodeMarketEventPayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	event, err := b.writeMarketEvent(ctx, request, payload, payloadRKey, eventRKey)
	if err != nil {
		return participant.Observation{}, err
	}
	if target := payload.submitTarget(request); target != "" {
		body := map[string]any{"uri": event.URI, "cid": event.CID, "record": event.Record}
		if refs := b.IngressRecords(ctx); len(refs) > 0 {
			body["ingress"] = refs
		}
		postCtx, cancel := context.WithTimeout(ctx, EventSubmitTimeout)
		err := b.outbound.Post(postCtx, target, market.SubmitEventNSID, body)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return participant.Observation{}, ctx.Err()
			}
			return participant.Observation{}, b.eventSubmitFailed(request, payload, target, err)
		}
		b.delivered.Store(deliveredKey(request.UID, request.Payload), true)
	}
	return b.observeEvent(ctx, request)
}

func (b *Bidder) writeMarketEvent(ctx context.Context, request participant.OutboxRequest, payload marketEventPayload, payloadRKey, eventRKey string) (market.SignedRecord, error) {
	stored, err := b.records.GetRecord(ctx, b.cfg.DID, market.EventNSID, eventRKey)
	if err == nil {
		return market.SignedRecord{URI: stored.URI, CID: stored.CID, Record: stored.Value}, nil
	}
	if !errors.Is(err, market.ErrNotFound) {
		return market.SignedRecord{}, fmt.Errorf("bidder: reading the market event: %w", err)
	}
	if payload.RequestEvent != nil {
		if err := b.applyInboundEvent(ctx, request, payload); err != nil {
			return market.SignedRecord{}, err
		}
	}
	record := unsignedRecord(payload.Record)
	signed, err := market.SignRecord(record, b.cfg.DID, b.signer)
	if err != nil {
		return market.SignedRecord{}, fmt.Errorf("bidder: signing the market event payload: %w", err)
	}
	writer := participant.CreateOnly{Records: b.records}
	written, err := writer.PutRecord(ctx, b.cfg.DID, payload.Collection, payloadRKey, signed)
	if err != nil {
		return market.SignedRecord{}, fmt.Errorf("bidder: writing the market event payload: %w", err)
	}
	event, err := market.PutEvent(ctx, writer, b.cfg.DID, eventRKey, market.EventOptions{
		Receipt:   market.RecordRef{URI: payload.Receipt.URI, CID: payload.Receipt.CID},
		Payload:   market.RecordRef{URI: written.URI, CID: written.CID},
		CreatedAt: recordObservedAt(record),
	}, b.signer)
	if err != nil {
		return market.SignedRecord{}, fmt.Errorf("bidder: writing the market event: %w", err)
	}
	return event, nil
}

func (b *Bidder) eventSubmitFailed(request participant.OutboxRequest, payload marketEventPayload, target string, err error) error {
	if participant.Refused(err) {
		b.log.Warn("the requester refused the market event", "collection", payload.Collection, "url", target, "err", err)
		return fmt.Errorf("bidder: the requester refused the market event: %w: %w", err, participant.ErrRejected)
	}
	sync, known := b.syncs.Find(func(sync v1alpha1.StateSync) bool { return string(sync.UID) == request.UID })
	if !b.eventWanted(request, payload) {
		b.log.Info("the market event is no longer sent: its ComputeVM is released", "collection", payload.Collection, "url", target,
			"stateSync", sync.Name, "err", err)
		return fmt.Errorf("bidder: the ComputeVM is released; the market event is no longer sent: %w: %w", err, participant.ErrObsolete)
	}
	wrapped := fmt.Errorf("bidder: submitting the market event to the requester: %w: %w", err, participant.ErrUnreachable)
	if !known || sync.Status.LastError != wrapped.Error() {
		b.log.Warn("submitting the market event to the requester failed; it is retried while the ComputeVM lives", "collection", payload.Collection,
			"url", target, "stateSync", sync.Name, "attempts", sync.Status.Attempts+1, "err", err)
	}
	return wrapped
}

func (b *Bidder) eventWanted(request participant.OutboxRequest, payload marketEventPayload) bool {
	if request.Source == nil {
		return false
	}
	vm, ok := b.vms.Get(request.Source.Name)
	if !ok || (request.Source.UID != "" && string(vm.UID) != request.Source.UID) {
		return false
	}
	if payload.Collection == market.ComputeEventsVMTerminatedNSID {
		return true
	}
	return vm.DeletionTimestamp == nil && !b.guests.Ended(&vm) && !b.guests.Releasing(&vm)
}

func (b *Bidder) deleteEvent(ctx context.Context, request participant.OutboxRequest) error {
	payload, _, err := decodeMarketEventPayload(request.Payload)
	if err != nil {
		return err
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	for _, item := range [][2]string{{payload.Collection, payloadRKey}, {market.EventNSID, eventRKey}} {
		if err := b.records.DeleteRecord(ctx, b.cfg.DID, item[0], item[1]); err != nil && !errors.Is(err, market.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (b *Bidder) verifyInboundDelete(ctx context.Context, event marketEventRequest, receiptURI, receiptCID, requesterDID string) (*market.Event, error) {
	uri := strings.TrimSpace(event.URI)
	if uri == "" || event.Record == nil {
		return nil, errors.New("bidder: the inbound event is empty")
	}
	parsed, err := market.ParseEvent(event.Record)
	if err != nil {
		return nil, fmt.Errorf("bidder: the inbound event is invalid: %w", err)
	}
	if market.NSIDFromURI(parsed.Payload.URI) != market.ComputeEventsVMDeleteNSID {
		return nil, errors.New("bidder: the inbound event is not a vm delete")
	}
	if err := b.verifier.VerifyRecord(ctx, market.ResolvedRecord{URI: uri, CID: event.CID, Value: event.Record}); err != nil {
		return nil, fmt.Errorf("bidder: the inbound event is not signed: %w", err)
	}
	if market.ATURIAuthority(uri) != requesterDID {
		return nil, errors.New("bidder: the inbound event is not from the requester")
	}
	if parsed.Receipt.URI != receiptURI || parsed.Receipt.CID != receiptCID {
		return nil, errors.New("bidder: the inbound event names another receipt")
	}
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID})
	if err != nil {
		return nil, fmt.Errorf("bidder: resolving the inbound event payload: %w", err)
	}
	if kind, _ := resolved.Value["$type"].(string); kind != market.ComputeEventsVMDeleteNSID {
		return nil, errors.New("bidder: the inbound event payload is not a vm delete")
	}
	if err := b.verifier.VerifyRecord(ctx, resolved); err != nil {
		return nil, fmt.Errorf("bidder: the inbound event payload is not signed: %w", err)
	}
	return parsed, nil
}

func (b *Bidder) applyInboundEvent(ctx context.Context, request participant.OutboxRequest, payload marketEventPayload) error {
	event, err := b.verifyInboundDelete(ctx, *payload.RequestEvent, payload.Receipt.URI, payload.Receipt.CID, payload.RequesterDID)
	if err != nil {
		return err
	}
	if request.Source == nil || strings.TrimSpace(request.Source.Name) == "" {
		return errors.New("bidder: the inbound event names no ComputeVM")
	}
	vm, ok := b.vms.Get(request.Source.Name)
	if !ok {
		b.log.Warn("the ComputeVM of an inbound delete event is gone", "computevm", request.Source.Name)
		return nil
	}
	if vm.Spec.Requester.Did != "" && vm.Spec.Requester.Did != payload.RequesterDID {
		return errors.New("bidder: the inbound event is not from the contract requester")
	}
	observedAt := b.stamp()
	release := v1alpha1.ComputeVMRelease{RequestedAt: observedAt}
	if event.Payload.URI != "" {
		release.Request = &commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID}
	}
	if err := b.intents.ApplyComputeVMRelease(ctx, vm.Name, release); err != nil {
		return fmt.Errorf("bidder: recording the release request on the ComputeVM: %w", err)
	}
	info := contractInfo{computeVM: vm.Name, receiptURI: payload.Receipt.URI, receiptCID: payload.Receipt.CID, acceptAuthor: payload.RequesterDID}
	if terms := vm.Spec.Contract; terms != nil {
		info.acceptURI, info.acceptCID, info.submitEventURL = terms.Accept.URI, terms.Accept.CID, terms.SubmitEventURL
	}
	if bid := b.bidForVM(&vm); bid != nil {
		info.bidName = bid.Name
	}
	b.markReleaseRequested(ctx, info.bidName, info,
		&commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID},
		&commonv1.StrongRef{URI: payload.RequestEvent.URI, CID: payload.RequestEvent.CID},
		observedAt)
	return nil
}

func decodeProvisionPayload(raw json.RawMessage) (provisionPayload, error) {
	var payload provisionPayload
	if len(raw) == 0 {
		return payload, errors.New("bidder: the provision payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("bidder: the provision payload is not JSON: %w", err)
	}
	switch {
	case payload.Accept.URI == "" || payload.Accept.CID == "":
		return payload, errors.New("bidder: the provision payload names no accept")
	case strings.TrimSpace(payload.RequesterDID) == "":
		return payload, errors.New("bidder: the provision payload names no requester")
	}
	return payload, nil
}

func (b *Bidder) settledObservation(ref string, request participant.OutboxRequest, meta map[string]string) participant.Observation {
	return participant.Observation{Exists: true, Ref: ref, Hash: payloadHash(request.Payload), At: b.stamp(), Meta: meta}
}

func (b *Bidder) observeProvision(_ context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, err := decodeProvisionPayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	acceptMeta := map[string]string{"acceptUri": payload.Accept.URI, "acceptCid": payload.Accept.CID}
	bid := b.bidForAcceptStatus(payload.Accept.URI, payload.Accept.CID)
	if bid != nil && bid.Status.Terminated != nil {
		return b.settledObservation("terminated:"+bid.Status.Terminated.ReceiptURI, request, acceptMeta), nil
	}
	var vm *v1alpha1.ComputeVM
	if bid != nil && bid.Status.ComputeVM != "" {
		if held, ok := b.vms.Get(bid.Status.ComputeVM); ok {
			vm = &held
		}
	}
	if vm == nil {
		key := b.guests.AcceptKey(payload.Accept.URI, payload.Accept.CID)
		if held, ok := b.vms.Find(func(vm v1alpha1.ComputeVM) bool { return b.guests.AcceptKeyOf(&vm) == key }); ok {
			vm = &held
		}
	}
	if vm == nil {
		if bid != nil && bid.Status.ComputeVM != "" {
			return b.settledObservation("released:"+bid.Status.ComputeVM, request, acceptMeta), nil
		}
		return participant.Observation{}, nil
	}
	terms := vm.Spec.Contract
	if terms == nil || terms.Accept.URI != payload.Accept.URI || terms.Accept.CID != payload.Accept.CID || terms.Receipt.URI == "" {
		return participant.Observation{}, nil
	}
	meta := map[string]string{"computevm": vm.Name, "acceptUri": payload.Accept.URI, "acceptCid": payload.Accept.CID,
		"receiptUri": terms.Receipt.URI, "receiptCid": terms.Receipt.CID}
	observation := b.settledObservation(vm.Name, request, meta)
	observation.CID = string(vm.UID)
	return observation, nil
}

func (b *Bidder) applyProvision(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, err := decodeProvisionPayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	acceptResolved, err := b.records.Resolve(ctx, market.RecordRef{URI: payload.Accept.URI, CID: payload.Accept.CID})
	if err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: resolving the accept: %w", err)
	}
	accept, err := market.ParseAccept(acceptResolved.Value)
	if err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: the accept is invalid: %w", err)
	}
	if err := b.verifier.VerifyRecord(ctx, acceptResolved); err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: the accept is not signed by its author: %w", err)
	}
	if scope := b.policy.Scope(ctx, participant.PolicyInput{SelfDid: b.cfg.DID, CounterpartyDid: payload.RequesterDID}); !scope.Allow {
		b.log.Info("the scope policy declined the accept", "accept", payload.Accept.URI, "reason", scope.Reason)
		return participant.Observation{}, errors.New("bidder: the scope policy declined the accept: " + scope.Reason)
	}
	found, err := b.bidForAccept(accept)
	if err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: finding the bid of the accept: %w", err)
	}
	unlock := b.lockBid(found.Name)
	defer unlock()
	bid, ok := b.bids.Get(found.Name)
	if !ok {
		return participant.Observation{}, fmt.Errorf("bidder: the bid %s is gone", found.Name)
	}
	contract, done, err := b.acceptContract(ctx, request, payload, accept, acceptResolved, &bid)
	if err != nil || done != nil {
		return ptrValue(done), err
	}
	name, err := b.provision(ctx, provisionRequest{
		rfpURI:         accept.RFP.URI,
		rfpCID:         accept.RFP.CID,
		vmURI:          contract.rfp.Payload.URI,
		vmCID:          contract.rfp.Payload.CID,
		acceptURI:      payload.Accept.URI,
		acceptCID:      payload.Accept.CID,
		bidURI:         accept.Bid.URI,
		bidCID:         accept.Bid.CID,
		requesterDID:   payload.RequesterDID,
		vmPayload:      contract.vmPayload,
		bidConfig:      contract.bidConfig,
		bidConfigValue: contract.bidConfigValue,
		owner:          bidOwner(&bid),
	})
	if err != nil {
		return participant.Observation{}, err
	}
	now := contract.now
	terms := v1alpha1.ComputeVMContract{
		Receipt:        commonv1.StrongRef{URI: contract.receiptURI, CID: contract.receiptCID},
		Accept:         commonv1.StrongRef{URI: payload.Accept.URI, CID: payload.Accept.CID},
		SubmitEventURL: contract.submitEventURL,
		AcceptedAt:     now.Format(time.RFC3339),
	}
	if err := b.intents.ApplyComputeVMContract(ctx, name, terms); err != nil {
		return participant.Observation{}, err
	}
	if held, ok := b.vms.Get(name); ok {
		held.Spec.Contract = &terms
		b.vms.Put(name, held)
	}
	if current, ok := b.bids.Get(bid.Name); ok {
		if err := b.bindAccepted(ctx, &current, name, now); err != nil {
			return participant.Observation{}, err
		}
		if terminated := current.Status.Terminated; terminated != nil {
			info := contractInfo{bidName: bid.Name, computeVM: name, receiptURI: contract.receiptURI, receiptCID: contract.receiptCID,
				acceptURI: payload.Accept.URI, acceptCID: payload.Accept.CID, acceptAuthor: payload.RequesterDID, submitEventURL: contract.submitEventURL}
			if err := b.releaseAccepted(ctx, info, terminated, now); err != nil {
				return participant.Observation{}, err
			}
		}
	}
	return b.observeProvision(ctx, request)
}

func ptrValue(o *participant.Observation) participant.Observation {
	if o == nil {
		return participant.Observation{}
	}
	return *o
}

type acceptedContract struct {
	rfp *market.RFP

	vmPayload market.ResolvedRecord

	bidConfig market.StrongRef

	bidConfigValue map[string]any

	submitEventURL string

	receiptURI string

	receiptCID string

	now time.Time
}

func (b *Bidder) acceptContract(ctx context.Context, request participant.OutboxRequest, payload provisionPayload, accept *market.Accept,
	acceptResolved market.ResolvedRecord, bid *v1alpha1.Bid) (acceptedContract, *participant.Observation, error) {
	var out acceptedContract
	if bid.Spec.Requester != "" && bid.Spec.Requester != payload.RequesterDID {
		return out, nil, errors.New("bidder: the accept is not from the requester that opened the contract")
	}
	if terminated := bid.Status.Terminated; terminated != nil {
		if bid.Status.Accept == nil || bid.Status.Accept.URI != payload.Accept.URI || bid.Status.Accept.CID != payload.Accept.CID {
			return out, nil, errors.New("bidder: the bid's contract was released")
		}
		settled := b.settledObservation("terminated:"+terminated.ReceiptURI, request,
			map[string]string{"acceptUri": payload.Accept.URI, "acceptCid": payload.Accept.CID})
		return out, &settled, nil
	}
	if bid.Status.Phase == v1alpha1.BidPhaseFailed {
		return out, nil, errors.New("bidder: the bid failed: " + bid.Status.Message)
	}
	out.now = b.now().UTC()
	if bidExpired(bid, out.now) || bid.Status.Phase == v1alpha1.BidPhaseExpired {
		b.expireBid(ctx, bid, out.now)
		return out, nil, errors.New("bidder: the bid has expired")
	}
	rfpResolved, err := b.records.Resolve(ctx, market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID})
	if err != nil {
		return out, nil, fmt.Errorf("bidder: resolving the accepted RFP: %w", err)
	}
	if out.rfp, err = market.ParseRFP(rfpResolved.Value); err != nil {
		return out, nil, fmt.Errorf("bidder: the accepted RFP is invalid: %w", err)
	}
	if err := b.verifier.VerifyRecord(ctx, rfpResolved); err != nil {
		return out, nil, fmt.Errorf("bidder: the accepted RFP is not signed by its author: %w", err)
	}
	decision := b.policy.EvaluatePolicies(ctx, out.rfp.Policies, b.fullInput(payload.RequesterDID, accept.RFP.URI, accept.RFP.CID))
	if !decision.Allow {
		b.log.Warn("policy rejected the accept", "accept", payload.Accept.URI, "reason", decision.Reason, "run", decision.Run)
		return out, nil, errors.New("bidder: the policy rejected the accept: " + decision.Reason)
	}
	if out.vmPayload, err = b.records.Resolve(ctx, market.RecordRef{URI: out.rfp.Payload.URI, CID: out.rfp.Payload.CID}); err != nil {
		return out, nil, fmt.Errorf("bidder: resolving the RFP payload: %w", err)
	}
	bidResolved, err := b.records.Resolve(ctx, market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID})
	if err != nil {
		return out, nil, fmt.Errorf("bidder: resolving the accepted bid: %w", err)
	}
	parsedBid, err := market.ParseBid(bidResolved.Value)
	if err != nil {
		return out, nil, fmt.Errorf("bidder: the accepted bid is invalid: %w", err)
	}
	if parsedBid.Config == nil {
		return out, nil, errors.New("bidder: the accepted bid names no bid config")
	}
	configResolved, err := b.records.Resolve(ctx, market.RecordRef{URI: parsedBid.Config.URI, CID: parsedBid.Config.CID})
	if err != nil {
		return out, nil, fmt.Errorf("bidder: resolving the bid config: %w", err)
	}
	out.bidConfig = market.StrongRef{URI: parsedBid.Config.URI, CID: parsedBid.Config.CID}
	out.bidConfigValue = configResolved.Value
	out.submitEventURL = b.eventEndpoint(ctx, payload.RequesterDID, accept.SubmitEvent)
	if strings.TrimSpace(accept.SubmitEvent) != "" && out.submitEventURL == "" {
		b.log.Warn("the accept's submitEvent does not resolve; the firehose carries the event", "accept", payload.Accept.URI, "submitEvent", accept.SubmitEvent)
	}
	receiptSubmitEvent := ""
	if b.cfg.InboundXRPC {
		receiptSubmitEvent = accept.SubmitEvent
	}
	out.receiptURI, out.receiptCID, err = b.createReceipt(ctx, market.ReceiptRefs{
		RFP:         market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID},
		Bid:         market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID},
		Accept:      market.RecordRef{URI: payload.Accept.URI, CID: payload.Accept.CID},
		Payload:     &market.RecordRef{URI: out.rfp.Payload.URI, CID: out.rfp.Payload.CID},
		SubmitEvent: receiptSubmitEvent,
		Ingress:     b.ingressRecords(ctx),
	}, acceptResolved, payload.RequesterDID)
	if err != nil {
		return out, nil, fmt.Errorf("bidder: writing the receipt: %w", err)
	}
	status := cloneBidStatus(bid.Status)
	if status.Accept == nil || status.Accept.URI != payload.Accept.URI || status.Accept.CID != payload.Accept.CID {
		status.Accept = &commonv1.StrongRef{URI: payload.Accept.URI, CID: payload.Accept.CID}
		if status.Timings.AcceptedAt == "" {
			status.Timings.AcceptedAt = out.now.Format(time.RFC3339)
		}
		status.Phase = v1alpha1.BidPhaseWon
		if err := b.writeBidStatus(ctx, bid, status); err != nil {
			return out, nil, fmt.Errorf("bidder: recording the accepted contract: %w", err)
		}
	}
	return out, nil, nil
}

func (b *Bidder) writeBidStatus(ctx context.Context, bid *v1alpha1.Bid, status v1alpha1.BidStatus) error {
	if err := b.intents.ApplyBidStatus(ctx, bid.Name, status); err != nil {
		return err
	}
	bid.Status = status
	b.bids.Put(bid.Name, *bid)
	return nil
}

func (b *Bidder) bindAccepted(ctx context.Context, bid *v1alpha1.Bid, vmName string, now time.Time) error {
	if bid.Status.ComputeVM == vmName {
		return nil
	}
	status := cloneBidStatus(bid.Status)
	status.ComputeVM = vmName
	status.Phase = v1alpha1.BidPhaseWon
	if status.Timings.AcceptedAt == "" {
		status.Timings.AcceptedAt = now.Format(time.RFC3339)
	}
	return b.writeBidStatus(ctx, bid, status)
}

func (b *Bidder) releaseAccepted(ctx context.Context, info contractInfo, terminated *v1alpha1.BidTerminated, now time.Time) error {
	release := v1alpha1.ComputeVMRelease{RequestedAt: terminated.At}
	var requestRef *commonv1.StrongRef
	if terminated.PayloadURI != "" {
		requestRef = &commonv1.StrongRef{URI: terminated.PayloadURI, CID: terminated.PayloadCid}
		release.Request = &commonv1.StrongRef{URI: terminated.PayloadURI, CID: terminated.PayloadCid}
	}
	if err := b.intents.ApplyComputeVMRelease(ctx, info.computeVM, release); err != nil {
		return err
	}
	if _, err := b.ensureTerminationSync(ctx, info, cmp.Or(terminated.At, now.Format(time.RFC3339)), requestRef); err != nil {
		b.log.Warn("resuming the termination of a released accept failed", "bid", info.bidName, "err", err.Error())
	}
	return nil
}

func decodeReleasePayload(raw json.RawMessage) (releasePayload, error) {
	var payload releasePayload
	if len(raw) == 0 {
		return payload, errors.New("bidder: the release payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("bidder: the release payload is not JSON: %w", err)
	}
	switch {
	case strings.TrimSpace(payload.ReceiptURI) == "":
		return payload, errors.New("bidder: the release payload names no receipt")
	case strings.TrimSpace(payload.RequesterDID) == "":
		return payload, errors.New("bidder: the release payload names no requester")
	case strings.TrimSpace(payload.Event.URI) == "" || payload.Event.Record == nil:
		return payload, errors.New("bidder: the release payload names no event")
	}
	return payload, nil
}

func releaseAppliedKey(payload releasePayload) string {
	return "release:" + payload.ReceiptURI + "#" + payload.Event.URI
}

func (b *Bidder) releaseSettled(payload releasePayload, request participant.OutboxRequest, ref string) participant.Observation {
	observation := b.settledObservation(cmp.Or(ref, "released:"+payload.ReceiptURI), request, nil)
	return observation
}

func (b *Bidder) observeRelease(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, err := decodeReleasePayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	if _, ok := b.released.Load(releaseAppliedKey(payload)); ok {
		return b.releaseSettled(payload, request, ""), nil
	}
	if market.ATURIAuthority(payload.ReceiptURI) != b.cfg.DID {
		return participant.Observation{}, nil
	}
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: payload.ReceiptURI})
	if errors.Is(err, market.ErrNotFound) {
		return participant.Observation{}, nil
	}
	if err != nil {
		return participant.Observation{}, err
	}
	receipt, err := market.ParseReceipt(resolved.Value)
	if err != nil {
		return participant.Observation{}, err
	}
	if vm := b.vmForAccept(receipt.Accept.URI, receipt.Accept.CID); vm != nil {
		if b.guests.ReleaseRequested(vm) {
			return b.releaseSettled(payload, request, vm.Name), nil
		}
		return participant.Observation{}, nil
	}
	bid := b.bidForAcceptStatus(receipt.Accept.URI, receipt.Accept.CID)
	if bid != nil && bid.Status.Terminated != nil && bid.Status.Terminated.ReceiptURI == payload.ReceiptURI {
		return b.releaseSettled(payload, request, ""), nil
	}
	return participant.Observation{}, nil
}

func (b *Bidder) applyRelease(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, err := decodeReleasePayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	if market.ATURIAuthority(payload.ReceiptURI) != b.cfg.DID {
		return participant.Observation{}, errors.New("bidder: the release intent names another bidder's receipt")
	}
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: payload.ReceiptURI})
	if err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: resolving the receipt of the release intent: %w", err)
	}
	receipt, err := market.ParseReceipt(resolved.Value)
	if err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: the receipt of the release intent is invalid: %w", err)
	}
	if market.ATURIAuthority(receipt.Accept.URI) != payload.RequesterDID {
		return participant.Observation{}, errors.New("bidder: the release intent is not from the accept author")
	}
	event, err := b.verifyInboundDelete(ctx, payload.Event, payload.ReceiptURI, resolved.CID, payload.RequesterDID)
	if err != nil {
		return participant.Observation{}, err
	}
	requestRef := &commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID}
	eventRef := &commonv1.StrongRef{URI: payload.Event.URI, CID: payload.Event.CID}
	observedAt := b.stamp()
	info := contractInfo{receiptURI: payload.ReceiptURI, receiptCID: resolved.CID,
		acceptURI: receipt.Accept.URI, acceptCID: receipt.Accept.CID, acceptAuthor: payload.RequesterDID,
		submitEventURL: b.eventEndpoint(ctx, payload.RequesterDID, receipt.SubmitEvent)}
	vm := b.vmForAccept(receipt.Accept.URI, receipt.Accept.CID)
	if vm == nil {
		bid := b.bidForAcceptStatus(receipt.Accept.URI, receipt.Accept.CID)
		if bid == nil {
			return participant.Observation{}, errors.New("bidder: the release intent names no known contract")
		}
		info.bidName = bid.Name
		b.markReleaseRequested(ctx, bid.Name, info, requestRef, eventRef, observedAt)
		b.released.Store(releaseAppliedKey(payload), true)
		return b.releaseSettled(payload, request, ""), nil
	}
	info.computeVM = vm.Name
	release := v1alpha1.ComputeVMRelease{RequestedAt: observedAt}
	if requestRef.URI != "" {
		release.Request = &commonv1.StrongRef{URI: requestRef.URI, CID: requestRef.CID}
	}
	if err := b.intents.ApplyComputeVMRelease(ctx, vm.Name, release); err != nil {
		return participant.Observation{}, fmt.Errorf("bidder: recording the release request on the ComputeVM: %w", err)
	}
	if bid := b.bidForVM(vm); bid != nil {
		info.bidName = bid.Name
	}
	b.markReleaseRequested(ctx, info.bidName, info, requestRef, eventRef, observedAt)
	if _, err := b.ensureTerminationSync(ctx, info, observedAt, requestRef); err != nil {
		return participant.Observation{}, err
	}
	b.released.Store(releaseAppliedKey(payload), true)
	return b.releaseSettled(payload, request, vm.Name), nil
}

type marketInboxPayload struct {
	DID string `json:"did"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Collection string `json:"collection"`

	Record map[string]any `json:"record"`
}

func (p *marketInboxPayload) decode(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("bidder: the market inbox payload is empty")
	}
	if err := json.Unmarshal(raw, p); err != nil {
		return fmt.Errorf("bidder: the market inbox payload is not JSON: %w", err)
	}
	if strings.TrimSpace(p.URI) == "" || strings.TrimSpace(p.CID) == "" {
		return errors.New("bidder: the market inbox payload names no record")
	}
	p.Collection = cmp.Or(strings.TrimSpace(p.Collection), market.NSIDFromURI(p.URI))
	return nil
}

func (b *Bidder) observeInbox(_ context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	var payload marketInboxPayload
	if err := payload.decode(request.Payload); err != nil {
		return participant.Observation{}, err
	}
	if _, ok := b.inboxed.Load(inboxKey(request)); ok {
		return participant.InboxObservation(request, payload.URI), nil
	}
	if payload.Collection == market.RFPNSID {
		if _, ok := b.bids.Get(bidName(b.cfg.DID, payload.URI, payload.CID)); ok {
			return participant.InboxObservation(request, payload.URI), nil
		}
	}
	return participant.Observation{At: b.stamp()}, nil
}

func (b *Bidder) applyInbox(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	var payload marketInboxPayload
	if err := payload.decode(request.Payload); err != nil {
		return participant.Observation{}, err
	}
	b.log.Info("market inbox applied", "collection", payload.Collection, "uri", payload.URI, "did", b.cfg.DID)
	if b.cfg.Suspend && payload.Collection == market.RFPNSID {
		b.log.Info("market inbox declined: the bidder's participation is suspended", "collection", payload.Collection, "uri", payload.URI)
		b.inboxed.Store(inboxKey(request), true)
		return participant.InboxObservation(request, payload.URI), nil
	}
	switch payload.Collection {
	case market.RFPNSID:
		if b.rfpClosed(payload.URI, payload.Record, "inbox") {
			break
		}
		if _, served := b.inboxServesKind(ctx, payload.URI, payload.CID); !served {
			b.log.Info("market inbox dropped an RFP of a compute kind this bidder does not serve", "rfp", payload.URI)
			b.inboxed.Store(inboxKey(request), true)
			return participant.InboxObservation(request, payload.URI), nil
		}
		bid, err := b.enqueueBid(ctx, payload.URI, payload.CID, payload.DID)
		if err != nil {
			return participant.Observation{}, fmt.Errorf("bidder: recording the bid intent: %w", err)
		}
		b.log.Info("market inbox enqueued a bid", "rfp", payload.URI, "bid", bid.Name, "phase", string(bid.Status.Phase))
	case market.AcceptNSID:
		if accept, err := market.ParseAccept(payload.Record); err == nil && market.ATURIAuthority(accept.Bid.URI) != b.cfg.DID {
			break
		}
		provision := provisionPayload{RequesterDID: payload.DID, Accept: provisionRef{URI: payload.URI, CID: payload.CID}}
		if _, err := b.ensureProvisionSync(ctx, provision); err != nil {
			return participant.Observation{}, fmt.Errorf("bidder: recording the provision intent: %w", err)
		}
	case market.EventNSID:
		if err := b.applyMarketEvent(ctx, payload.URI, payload.CID, payload.Record, payload.DID); err != nil {
			return participant.Observation{}, err
		}
	}
	b.inboxed.Store(inboxKey(request), true)
	return participant.InboxObservation(request, payload.URI), nil
}

func inboxKey(request participant.OutboxRequest) string {
	return request.UID + "#" + request.Key
}

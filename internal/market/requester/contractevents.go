package requester

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const (
	ContractEventSubmitRetry = 2 * time.Second

	ContractEventSubmitRetryCap = time.Minute

	ContractEventSubmitTimeout = 10 * time.Second

	inboundContractEventPrefix = "in-"

	maxGuestKey = 256

	maxGuestCiphertext = 16384

	ageArmorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"
)

var _ contractmarket.ContractEventObserver = (*Requester)(nil)

type guestPayload struct {
	Kind string

	Tag string

	Key string

	Ciphertext string

	CreatedAt string
}

func InboundContractEventName(eventURI string) string {
	sum := sha256.Sum256([]byte(eventURI))
	return inboundContractEventPrefix + hex.EncodeToString(sum[:10])
}

func validateGuestFields(nsid, kind, tag, key, ciphertext string) error {
	if nsid == market.ComputeEventsGuestSecretsNSID {
		switch {
		case kind != "" || tag != "" || key != "":
			return errors.New("a secrets event carries only a ciphertext")
		case len(ciphertext) > market.MaxGuestSecretsCiphertext || !strings.HasPrefix(strings.TrimSpace(ciphertext), ageArmorHeader):
			return fmt.Errorf("ciphertext is not age armored ciphertext of at most %d bytes", market.MaxGuestSecretsCiphertext)
		}
		return nil
	}
	if nsid == market.ComputeEventsGuestAgentStartNSID {
		if kind != "" || tag != "" || key != "" || ciphertext != "" {
			return errors.New("an agentStart event carries no kind, tag, key or ciphertext")
		}
		return nil
	}
	if !networkTagRE.MatchString(tag) {
		return fmt.Errorf("tag %q is not a DNS label of at most 32 characters", tag)
	}
	switch nsid {
	case market.ComputeEventsGuestPublicKeyNSID:
		switch {
		case kind != market.GuestPublicKeyKindAgeX25519:
			return fmt.Errorf("kind %q is not %s", kind, market.GuestPublicKeyKindAgeX25519)
		case key == "" || len(key) > maxGuestKey || !strings.HasPrefix(key, "age1") || strings.ContainsAny(key, " \t\r\n"):
			return fmt.Errorf("key is not an age X25519 recipient (age1..., at most %d bytes)", maxGuestKey)
		case ciphertext != "":
			return errors.New("a publicKey carries no ciphertext")
		}
	case market.ComputeEventsGuestSignalWebRTCNSID:
		switch {
		case kind != market.GuestSignalKindOffer && kind != market.GuestSignalKindAnswer:
			return fmt.Errorf("kind %q is not %s or %s", kind, market.GuestSignalKindOffer, market.GuestSignalKindAnswer)
		case len(ciphertext) > maxGuestCiphertext || !strings.HasPrefix(strings.TrimSpace(ciphertext), ageArmorHeader):
			return fmt.Errorf("ciphertext is not age armored ciphertext of at most %d bytes", maxGuestCiphertext)
		case key != "":
			return errors.New("a signalWebRTC carries no key")
		}
	default:
		return fmt.Errorf("nsid %q is not a guest event", nsid)
	}
	return nil
}

func parseGuestPayload(nsid string, value map[string]any) (guestPayload, error) {
	var out guestPayload
	switch nsid {
	case market.ComputeEventsGuestPublicKeyNSID:
		record, err := market.ParseComputeEventsGuestPublicKey(value)
		if err != nil {
			return out, err
		}
		out = guestPayload{Kind: record.Kind, Tag: record.Tag, Key: record.Key, CreatedAt: record.CreatedAt}
	case market.ComputeEventsGuestSignalWebRTCNSID:
		record, err := market.ParseComputeEventsGuestSignalWebRTC(value)
		if err != nil {
			return out, err
		}
		out = guestPayload{Kind: record.Kind, Tag: record.Tag, Ciphertext: record.Ciphertext, CreatedAt: record.CreatedAt}
	default:
		return out, fmt.Errorf("nsid %q is not a guest event", nsid)
	}
	if err := validateGuestFields(nsid, out.Kind, out.Tag, out.Key, out.Ciphertext); err != nil {
		return out, err
	}
	if _, err := market.ParseTime(out.CreatedAt); err != nil {
		return out, fmt.Errorf("createdAt: %w", err)
	}
	return out, nil
}

func outboundKind(spec v1alpha1.ContractEventSpec) string {
	if spec.NSID == market.ComputeEventsGuestPublicKeyNSID {
		return cmp.Or(spec.Kind, market.GuestPublicKeyKindAgeX25519)
	}
	return spec.Kind
}

func (q *Requester) ObserveContractEvent(ctx context.Context, change contractmarket.ContractEventChange) (time.Duration, error) {
	if change.Gone || change.Event == nil {
		q.contractEvents.Remove(change.Name)
		return 0, nil
	}
	event := *change.Event
	if prev, ok := q.contractEvents.Get(event.Name); ok && (prev.UID == "" || prev.UID == event.UID) && statusAhead(prev.Status, event.Status) {
		event.Status = *prev.Status.DeepCopy()
	}
	q.contractEvents.Put(event.Name, event)
	if q.stopping() || q.deps.ContractEvents == nil || event.DeletionTimestamp != nil {
		return 0, nil
	}
	if contract := change.Contract; contract != nil && contract.UID != "" && !ownedBy(event, *contract) {
		if err := q.deps.ContractEvents.ApplyContractEventOwner(ctx, event.Name, *contract); err != nil {
			return 0, err
		}
		event.OwnerReferences = append(event.OwnerReferences, contractOwnerReference(*contract))
		q.contractEvents.Put(event.Name, event)
	}
	if event.Spec.Direction == v1alpha1.ContractEventInbound {
		if err := q.observeInboundContractEvent(ctx, event); err != nil {
			return 0, err
		}
		if contract := change.Contract; contract != nil && guestSecretsKey(event) {
			return 0, q.sealGuestSecrets(ctx, *contract)
		}
		return 0, nil
	}
	return q.observeOutboundContractEvent(ctx, event, change.Contract)
}

func contractEventRank(status v1alpha1.ContractEventStatus) int {
	switch status.Phase {
	case v1alpha1.ContractEventSubmitted, v1alpha1.ContractEventReceived, v1alpha1.ContractEventFailed:
		return 3
	case v1alpha1.ContractEventPublished:
		return 2
	}
	if status.Record != nil {
		return 2
	}
	return 1
}

func submitAttempts(status v1alpha1.ContractEventStatus) int32 {
	if status.Submit == nil {
		return 0
	}
	return status.Submit.Attempts
}

func statusAhead(known, seen v1alpha1.ContractEventStatus) bool {
	if known.ObservedGeneration != seen.ObservedGeneration {
		return known.ObservedGeneration > seen.ObservedGeneration
	}
	if a, b := contractEventRank(known), contractEventRank(seen); a != b {
		return a > b
	}
	return submitAttempts(known) > submitAttempts(seen)
}

func (q *Requester) writeContractEventStatus(ctx context.Context, event *v1alpha1.ContractEvent, status v1alpha1.ContractEventStatus) error {
	status.ObservedGeneration = event.Generation
	if apiequality.Semantic.DeepEqual(event.Status, status) {
		return nil
	}
	if err := q.deps.ContractEvents.ApplyContractEventStatus(ctx, event.Name, status); err != nil {
		return err
	}
	event.Status = *status.DeepCopy()
	q.contractEvents.Put(event.Name, *event)
	return nil
}

func (q *Requester) observeInboundContractEvent(ctx context.Context, event v1alpha1.ContractEvent) error {
	if event.Status.Record != nil {
		return nil
	}
	if _, creating := q.creatingInbound.Load(event.Name); creating || event.Status.Phase == v1alpha1.ContractEventFailed {
		return nil
	}
	status := *event.Status.DeepCopy()
	status.Phase = v1alpha1.ContractEventFailed
	status.Message = "an inbound ContractEvent is written by the requester from a bidder event; create an outbound one"
	q.log.Warn("requester_contract_event_refused", "contractEvent", event.Name, "reason", status.Message)
	return q.writeContractEventStatus(ctx, &event, status)
}

func (q *Requester) observeOutboundContractEvent(ctx context.Context, event v1alpha1.ContractEvent, contract *v1alpha1.ComputeContract) (time.Duration, error) {
	respec := event.Status.ObservedGeneration != 0 && event.Status.ObservedGeneration != event.Generation
	if !respec && (event.Status.Phase == v1alpha1.ContractEventFailed || event.Status.Phase == v1alpha1.ContractEventSubmitted) {
		return 0, nil
	}
	now := q.now()
	status := *event.Status.DeepCopy()
	if respec {
		status = v1alpha1.ContractEventStatus{Conditions: status.Conditions}
	}
	fail := func(message string) (time.Duration, error) {
		status.Phase = v1alpha1.ContractEventFailed
		status.Message = message
		if status.Record == nil {
			conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionPublished, metav1.ConditionFalse, "Failed", message, now)
		}
		conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionSubmitted, metav1.ConditionFalse, "Failed", message, now)
		q.log.Warn("requester_contract_event_failed", "contractEvent", event.Name, "contract", event.Spec.ContractRef.Name, "reason", message)
		return 0, q.writeContractEventStatus(ctx, &event, status)
	}
	wait := func(message string) (time.Duration, error) {
		status.Phase = v1alpha1.ContractEventPending
		status.Message = message
		conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionPublished, metav1.ConditionFalse, "Waiting", message, now)
		return 0, q.writeContractEventStatus(ctx, &event, status)
	}
	spec := event.Spec
	if err := validateGuestFields(spec.NSID, outboundKind(spec), spec.Tag, spec.Key, spec.Ciphertext); err != nil {
		return fail("the spec is invalid: " + err.Error())
	}
	name := strings.TrimSpace(spec.ContractRef.Name)
	if contract == nil {
		if status.Record != nil {
			return fail("the ComputeContract " + name + " is gone")
		}
		return wait("waiting for the ComputeContract " + name)
	}
	if contract.DeletionTimestamp != nil {
		return fail("the ComputeContract " + name + " is being deleted")
	}
	if spec.NSID == market.ComputeEventsGuestAgentStartNSID && contract.Spec.Agent == nil {
		return fail("the ComputeContract " + name + " has no spec.agent; an agentStart starts nothing")
	}
	state := contract.Status.Contract
	switch contract.Status.Phase {
	case v1alpha1.ComputeContractReleased, v1alpha1.ComputeContractFailed:
		return fail("the ComputeContract " + name + " is " + string(contract.Status.Phase))
	case v1alpha1.ComputeContractHeld, v1alpha1.ComputeContractReleasing:
	default:
		if status.Record == nil {
			return wait("waiting for the ComputeContract " + name + " to be held")
		}
	}
	if state == nil || state.Receipt == nil || state.Receipt.URI == "" || state.Accept == nil || state.Accept.URI == "" {
		return wait("waiting for the receipt and the accept of the ComputeContract " + name)
	}
	envelope, err := q.publishContractEvent(ctx, event, state)
	if err != nil {
		return 0, fmt.Errorf("requester: publishing the ContractEvent %s: %w", event.Name, err)
	}
	if status.Record == nil || status.Event == nil || status.Phase != v1alpha1.ContractEventPublished {
		status.Record = &commonv1.StrongRef{URI: envelope.payload.URI, CID: envelope.payload.CID}
		status.Event = &commonv1.StrongRef{URI: envelope.event.URI, CID: envelope.event.CID}
		status.Phase = v1alpha1.ContractEventPublished
		status.Message = "the record and its market.event are in the requester's repo"
		conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionPublished, metav1.ConditionTrue, "Published", status.Message, now)
		if err := q.writeContractEventStatus(ctx, &event, status); err != nil {
			return 0, err
		}
		q.log.Info("requester_contract_event_published", "contractEvent", event.Name, "nsid", spec.NSID, "tag", spec.Tag,
			"record", status.Record.URI, "event", status.Event.URI)
	}
	return q.submitContractEvent(ctx, &event, status, state, envelope.event)
}

func ownedBy(event v1alpha1.ContractEvent, contract v1alpha1.ComputeContract) bool {
	for _, ref := range event.OwnerReferences {
		if ref.Kind == "ComputeContract" && ref.Name == contract.Name && ref.UID == contract.UID {
			return true
		}
	}
	return false
}

func contractOwnerReference(contract v1alpha1.ComputeContract) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeContract", Name: contract.Name, UID: contract.UID}
}

type publishedContractEvent struct {
	payload market.SignedRecord

	event market.SignedRecord
}

func (q *Requester) publishContractEvent(ctx context.Context, event v1alpha1.ContractEvent, state *v1alpha1.NodeClaimContract) (publishedContractEvent, error) {
	var out publishedContractEvent
	uid := string(event.UID)
	if uid == "" {
		return out, errors.New("the ContractEvent has no UID")
	}
	nsid := event.Spec.NSID
	if event.Generation > 1 {
		uid += "-g" + strconv.FormatInt(event.Generation, 10)
	}
	payloadRKey := sessionRKey("contract-event", uid)
	payload, found, err := q.existingSignedRecord(ctx, nsid, payloadRKey)
	if err != nil {
		return out, fmt.Errorf("read the payload record: %w", err)
	}
	if !found {
		record, err := q.contractEventRecord(event)
		if err != nil {
			return out, err
		}
		payload, err = market.CreateSignedRecordAt(ctx, q.repo, q.repoDID, nsid, payloadRKey, record, q.signer)
		if err != nil {
			return out, fmt.Errorf("create the payload record: %w", err)
		}
	}
	eventRKey := sessionRKey("contract-event-envelope", uid)
	envelope, found, err := q.existingSignedRecord(ctx, market.EventNSID, eventRKey)
	if err != nil {
		return out, fmt.Errorf("read the market.event: %w", err)
	}
	if !found {
		envelope, err = market.PutEvent(ctx, q.repo, q.repoDID, eventRKey, market.EventOptions{
			Receipt:   market.RecordRef{URI: state.Receipt.URI, CID: state.Receipt.CID},
			Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
			CreatedAt: q.nowISO(),
		}, q.signer)
		if err != nil {
			return out, fmt.Errorf("create the market.event: %w", err)
		}
	}
	return publishedContractEvent{payload: payload, event: envelope}, nil
}

func (q *Requester) contractEventRecord(event v1alpha1.ContractEvent) (map[string]any, error) {
	spec := event.Spec
	switch spec.NSID {
	case market.ComputeEventsGuestPublicKeyNSID:
		return market.EncodeRecord(market.ComputeEventsGuestPublicKey{Kind: outboundKind(spec), Tag: spec.Tag, Key: spec.Key, CreatedAt: q.nowISO()})
	case market.ComputeEventsGuestSignalWebRTCNSID:
		return market.EncodeRecord(market.ComputeEventsGuestSignalWebRTC{Kind: spec.Kind, Tag: spec.Tag, Ciphertext: spec.Ciphertext, CreatedAt: q.nowISO()})
	case market.ComputeEventsGuestSecretsNSID:
		return market.EncodeRecord(market.ComputeEventsGuestSecrets{Ciphertext: spec.Ciphertext, CreatedAt: q.nowISO()})
	case market.ComputeEventsGuestAgentStartNSID:
		return market.EncodeRecord(market.ComputeEventsGuestAgentStart{CreatedAt: q.nowISO()})
	}
	return nil, fmt.Errorf("nsid %q is not a guest event", spec.NSID)
}

func contractEventSubmitDelay(attempts int32) time.Duration {
	return min(ContractEventSubmitRetry<<min(max(attempts-1, 0), 5), ContractEventSubmitRetryCap)
}

func (q *Requester) submitContractEvent(ctx context.Context, event *v1alpha1.ContractEvent, status v1alpha1.ContractEventStatus,
	state *v1alpha1.NodeClaimContract, envelope market.SignedRecord) (time.Duration, error) {
	now := q.now().UTC()
	status = *status.DeepCopy()
	submit := v1alpha1.ContractEventSubmit{}
	if status.Submit != nil {
		submit = *status.Submit
	}
	if submit.SubmittedAt != "" {
		return 0, nil
	}
	if submit.Attempts > 0 {
		if last, err := time.Parse(time.RFC3339, submit.LastAttemptAt); err == nil {
			if left := last.Add(contractEventSubmitDelay(submit.Attempts)).Sub(now); left > 0 {
				return left, nil
			}
		}
	}
	bidderDID := winnerDID(state)
	bidder, ok := q.winnerBidder(ctx, state, bidderDID)
	if !ok {
		bidder, ok = q.bidderForDID(ctx, bidderDID)
	}
	var err error
	code := 0
	if !ok || bidder.Endpoint == "" {
		err = fmt.Errorf("no submitEvent endpoint is known for the bidder %s", bidderDID)
	} else {
		submit.Endpoint = bidder.Endpoint
		submitCtx, cancel := context.WithTimeout(ctx, ContractEventSubmitTimeout)
		code, err = q.market.SubmitEvent(submitCtx, bidder, SubmitEventRequest{URI: envelope.URI, CID: envelope.CID, Record: envelope.Record})
		cancel()
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
	previous := submit.LastError
	submit.Attempts++
	submit.LastAttemptAt = now.Format(time.RFC3339)
	status.Submit = &submit
	switch {
	case err == nil && code >= 200 && code <= 299:
		submit.SubmittedAt = submit.LastAttemptAt
		submit.LastError = ""
		status.Phase = v1alpha1.ContractEventSubmitted
		status.Message = "submitted to " + bidder.Endpoint
		conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionSubmitted, metav1.ConditionTrue, "Submitted", status.Message, now)
		if err := q.writeContractEventStatus(ctx, event, status); err != nil {
			return 0, err
		}
		q.log.Info("requester_contract_event_submitted", "contractEvent", event.Name, "endpoint", bidder.Endpoint, "attempts", submit.Attempts,
			"event", envelope.URI)
		return 0, nil
	case participant.Refused(err):
		submit.LastError = err.Error()
		status.Phase = v1alpha1.ContractEventFailed
		status.Message = "the bidder refused the event: " + submit.LastError
		conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionSubmitted, metav1.ConditionFalse, "Refused", status.Message, now)
		q.log.Warn("requester_contract_event_refused_by_bidder", "contractEvent", event.Name, "endpoint", bidder.Endpoint, "status", code,
			"err", err)
		return 0, q.writeContractEventStatus(ctx, event, status)
	case err == nil:
		submit.LastError = fmt.Sprintf("HTTP %d", code)
	default:
		submit.LastError = err.Error()
	}
	delay := contractEventSubmitDelay(submit.Attempts)
	if submit.LastError != previous {
		q.log.Warn("requester_contract_event_submit_retry", "contractEvent", event.Name, "endpoint", submit.Endpoint, "attempts", submit.Attempts,
			"retryIn", delay.String(), "err", submit.LastError)
	}
	status.Message = "submitting to the bidder: " + submit.LastError
	conds.SetStatusAt(&status.Conditions, event.Generation, v1alpha1.ContractEventConditionSubmitted, metav1.ConditionFalse, "Retrying", submit.LastError, now)
	if err := q.writeContractEventStatus(ctx, event, status); err != nil {
		return 0, err
	}
	return delay, nil
}

func (q *Requester) recordInboundContractEvent(ctx context.Context, contract v1alpha1.ComputeContract, parsed *GuestEvent) error {
	handle := q.deps.ContractEvents
	if handle == nil {
		return nil
	}
	name := InboundContractEventName(parsed.Event.URI)
	spec := v1alpha1.ContractEventSpec{
		ContractRef: commonv1.LocalObjectRef{Name: contract.Name},
		Direction:   v1alpha1.ContractEventInbound,
		NSID:        parsed.Kind,
		Kind:        parsed.GuestKind,
		Tag:         parsed.Tag,
		Key:         parsed.Key,
		Ciphertext:  parsed.Ciphertext,
	}
	record := commonv1.StrongRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID}
	envelope := commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID}
	existing, known := q.contractEvents.Get(name)
	if known && existing.Status.Phase == v1alpha1.ContractEventReceived && existing.Status.Record != nil && *existing.Status.Record == record {
		return nil
	}
	q.creatingInbound.Store(name, true)
	defer q.creatingInbound.Delete(name)
	if known && !apiequality.Semantic.DeepEqual(existing.Spec, spec) {
		q.log.Warn("requester_contract_event_replaced", "contractEvent", name, "reason", "its spec does not match the bidder event")
		if err := handle.DeleteContractEvent(ctx, name); err != nil {
			return err
		}
		return fmt.Errorf("requester: the ContractEvent %s held another spec; it is written again", name)
	}
	if !known {
		err := handle.CreateContractEvent(ctx, v1alpha1.ContractEvent{
			ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{contractOwnerReference(contract)}},
			Spec:       spec,
		})
		if err != nil && !errors.Is(err, contractmarket.ErrExists) {
			return err
		}
	}
	now := q.now()
	status := v1alpha1.ContractEventStatus{
		Phase:   v1alpha1.ContractEventReceived,
		Message: "received from the bidder " + market.ATURIAuthority(parsed.Event.URI),
		Record:  &record,
		Event:   &envelope,
	}
	if known {
		status.Conditions = existing.Status.DeepCopy().Conditions
	}
	stored := v1alpha1.ContractEvent{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1, CreationTimestamp: metav1.NewTime(now)}, Spec: spec}
	if known {
		stored = existing
	}
	conds.SetStatusAt(&status.Conditions, stored.Generation, v1alpha1.ContractEventConditionPublished, metav1.ConditionTrue, "Received", status.Message, now)
	status.ObservedGeneration = stored.Generation
	if err := handle.ApplyContractEventStatus(ctx, name, status); err != nil {
		return err
	}
	stored.Status = status
	q.contractEvents.Put(name, stored)
	q.log.Info("requester_contract_event_received", "contractEvent", name, "contract", contract.Name, "nsid", parsed.Kind, "tag", parsed.Tag,
		"record", record.URI)
	return nil
}

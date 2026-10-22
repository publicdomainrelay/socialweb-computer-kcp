package requester

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

const (
	InboxAdapter = "requester.inbox"

	inboxLabel = "socialweb.computer/inbox"

	inboxPrefix = "inbox-"

	maxPendingEvents = 256
)

var inboxGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "statesyncs"}

type inboxEventPayload struct {
	Event v1alpha1.ComputeContractEvent `json:"event"`
}

func sameEventRef(a, b v1alpha1.ComputeContractEvent) bool {
	return a.URI != "" && a.URI == b.URI && (a.CID == "" || b.CID == "" || a.CID == b.CID)
}

func (s *Service) enqueueEvent(ctx context.Context, event v1alpha1.ComputeContractEvent) error {
	if event.URI == "" || event.CID == "" || event.ReceiptURI == "" {
		return errors.New("requester: the event ref is incomplete")
	}
	contract, err := s.contractForReceiptURI(ctx, event.ReceiptURI)
	if err != nil {
		return err
	}
	if contract == nil {
		return s.storeInboxEvent(ctx, event)
	}
	for _, pending := range contract.Status.PendingEvents {
		if sameEventRef(pending, event) {
			return nil
		}
	}
	if len(contract.Status.PendingEvents) >= maxPendingEvents {
		return errors.New("requester: the pending event inbox is full")
	}
	return s.contracts.AppendPendingEvent(ctx, contract.Name, event)
}

func (s *Service) contractForReceiptURI(ctx context.Context, receiptURI string) (*v1alpha1.ComputeContract, error) {
	if s.contracts == nil {
		return nil, errors.New("requester: no ComputeContract store is configured")
	}
	contracts, err := s.contracts.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range contracts {
		state := contracts[i].Status.Contract
		if state == nil || state.Receipt == nil {
			continue
		}
		if state.Receipt.URI == receiptURI {
			return &contracts[i], nil
		}
	}
	return nil, nil
}

func inboxName(event v1alpha1.ComputeContractEvent) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(event.ReceiptURI) + "\x00" + event.URI + "#" + event.CID))
	return inboxPrefix + hex.EncodeToString(sum[:16])
}

func (s *Service) storeInboxEvent(ctx context.Context, event v1alpha1.ComputeContractEvent) error {
	if s.inbox == nil {
		return errors.New("requester: the event inbox store is not configured")
	}
	payload, err := json.Marshal(inboxEventPayload{Event: event})
	if err != nil {
		return err
	}
	object := &v1alpha1.StateSync{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "StateSync"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   inboxName(event),
			Labels: map[string]string{inboxLabel: "true"},
		},
		Spec: v1alpha1.StateSyncSpec{
			Sink:           v1alpha1.StateSyncSink{Adapter: InboxAdapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        payload,
			IdempotencyKey: "inbox:" + event.URI + "#" + event.CID,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
	err = s.inbox.Create(ctx, s.cfg.Workspace, object)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (s *Service) mergeInboxMailboxes(ctx context.Context) {
	if s.inbox == nil {
		return
	}
	items, err := s.inbox.List(ctx, s.cfg.Workspace)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("requester_inbox_list_failed", "err", err)
		}
		return
	}
	for i := range items {
		mailbox := &items[i]
		if mailbox.Spec.Sink.Adapter != InboxAdapter {
			continue
		}
		var payload inboxEventPayload
		if err := json.Unmarshal(mailbox.Spec.Payload, &payload); err != nil || payload.Event.URI == "" {
			s.deleteInboxMailbox(ctx, mailbox.Name)
			continue
		}
		contract, err := s.contractForReceiptURI(ctx, payload.Event.ReceiptURI)
		if err != nil || contract == nil {
			continue
		}
		found := false
		for _, pending := range contract.Status.PendingEvents {
			if sameEventRef(pending, payload.Event) {
				found = true
				break
			}
		}
		if !found {
			if len(contract.Status.PendingEvents) >= maxPendingEvents {
				s.log.Warn("requester_inbox_full", "contract", contract.Name, "event", payload.Event.URI)
				continue
			}
			if err := s.contracts.AppendPendingEvent(ctx, contract.Name, payload.Event); err != nil {
				if ctx.Err() == nil {
					s.log.Warn("requester_inbox_append_failed", "contract", contract.Name, "event", payload.Event.URI, "err", err)
				}
				continue
			}
		}
		s.deleteInboxMailbox(ctx, mailbox.Name)
	}
}

func (s *Service) deleteInboxMailbox(ctx context.Context, name string) {
	if s.inbox == nil {
		return
	}
	if err := s.inbox.Delete(ctx, ref.New(s.cfg.Workspace, "", name)); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("requester_inbox_delete_failed", "statesync", name, "err", err)
		}
	}
}

func (s *Service) resolveInboxEvent(ctx context.Context, event v1alpha1.ComputeContractEvent) (MarketEvent, error) {
	if s.repo == nil {
		return MarketEvent{}, errors.New("requester: the repo is not open")
	}
	resolved, err := s.repo.Resolve(ctx, market.RecordRef{URI: event.URI, CID: event.CID})
	if err != nil {
		return MarketEvent{}, err
	}
	envelope, err := market.ParseEvent(resolved.Value)
	if err != nil {
		return MarketEvent{}, err
	}
	payload, err := s.repo.Resolve(ctx, market.RecordRef{URI: envelope.Payload.URI, CID: envelope.Payload.CID})
	if err != nil {
		return MarketEvent{}, err
	}
	return MarketEvent{Event: resolved, Payload: payload}, nil
}

func guestEventApplied(state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	if state == nil || parsed == nil {
		return false
	}
	switch parsed.Kind {
	case market.ComputeEventsVMOnNetworkNSID:
		return state.VMAddress != ""
	case market.ComputeEventsVMHostKeysNSID:
		return state.HostKeys != nil && guestEventRecorded(state.HostKeys.Event, state.HostKeys.Payload, parsed)
	case market.ComputeEventsVMTerminatedNSID:
		return terminationRecorded(state, parsed.Termination)
	}
	return false
}

func samePendingEvents(left, right []v1alpha1.ComputeContractEvent) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !sameEventRef(left[i], right[i]) {
			return false
		}
	}
	return true
}

func (s *Service) runnerFor(name string) *contractRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runners[name]
}

func (s *Service) processPendingEvents(ctx context.Context) {
	if s.contracts == nil {
		return
	}
	s.mergeInboxMailboxes(ctx)
	contracts, err := s.contracts.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("requester_event_inbox_list_failed", "err", err)
		}
		return
	}
	for i := range contracts {
		contract := &contracts[i]
		if len(contract.Status.PendingEvents) == 0 {
			continue
		}
		remaining := make([]v1alpha1.ComputeContractEvent, 0, len(contract.Status.PendingEvents))
		seen := map[string]bool{}
		for _, entry := range contract.Status.PendingEvents {
			key := entry.URI + "#" + entry.CID
			if seen[key] {
				continue
			}
			seen[key] = true
			event, err := s.resolveInboxEvent(ctx, entry)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Warn("requester_event_unavailable", "contract", contract.Name, "event", entry.URI, "err", err)
				remaining = append(remaining, entry)
				continue
			}
			receiptURI := strings.TrimSpace(entry.ReceiptURI)
			parsed, err := parseGuestEvent(ctx, s.directory, s.badgeBlueBinding(), event, receiptURI)
			if err != nil {
				s.log.Warn("requester_event_refused", "contract", contract.Name, "event", entry.URI, "err", err)
				continue
			}
			if guestEventApplied(contract.Status.Contract, parsed) {
				continue
			}
			if runner := s.runnerFor(contract.Name); runner != nil {
				s.dispatchEvent(receiptURI, event)
				remaining = append(remaining, entry)
				continue
			}
			if contractPhaseDone(contract.Status.Phase) {
				s.log.Info("requester_event_dropped_after_contract_end", "contract", contract.Name, "event", entry.URI)
				continue
			}
			remaining = append(remaining, entry)
		}
		if samePendingEvents(remaining, contract.Status.PendingEvents) {
			continue
		}
		if err := s.contracts.SetPendingEvents(ctx, contract.Name, remaining); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("requester_event_inbox_update_failed", "contract", contract.Name, "err", err)
			}
		}
	}
}

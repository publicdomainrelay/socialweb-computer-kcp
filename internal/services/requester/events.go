package requester

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type GuestEvent struct {
	Kind string

	Event market.ResolvedRecord

	Payload market.ResolvedRecord

	Address string

	HostKeys []v1alpha1.ComputeVMHostKey

	Termination *v1alpha1.ContractTermination

	ObservedAt string
}

func ValidateTicket(address string) (string, error) {
	ticket := strings.TrimSpace(address)
	ticket = strings.TrimPrefix(ticket, "iroh://")
	if ticket == "" || strings.Contains(ticket, "://") || strings.ContainsAny(ticket, " \t\r\n/") {
		return "", fmt.Errorf("requester: %q is not an iroh ticket", address)
	}
	if err := irohdial.CheckTicket(ticket); err != nil {
		return "", fmt.Errorf("requester: %q is not an iroh ticket: %w", address, err)
	}
	return ticket, nil
}

func parseGuestEvent(ctx context.Context, dir identity.Directory, badge func(context.Context, string, string) (bool, error), event MarketEvent, receiptURI string) (*GuestEvent, error) {
	if event.Event.URI == "" || event.Payload.URI == "" {
		return nil, errors.New("requester: the market event has no event or payload record")
	}
	envelope, err := market.ParseEvent(event.Event.Value)
	if err != nil {
		return nil, err
	}
	payloadNSID := market.NSIDFromURI(event.Payload.URI)
	switch payloadNSID {
	case market.ComputeEventsVMOnNetworkNSID, market.ComputeEventsVMHostKeysNSID, market.ComputeEventsVMTerminatedNSID:
	default:
		return nil, fmt.Errorf("requester: payload %s is not a guest event", payloadNSID)
	}
	ref, err := market.BindEventPayload(*envelope, event.Event.URI, receiptURI, payloadNSID)
	if err != nil {
		return nil, err
	}
	if market.ATURIAuthority(event.Event.URI) != market.ATURIAuthority(receiptURI) {
		return nil, fmt.Errorf("requester: event author is not the bidder")
	}
	keys := market.NewDirectoryKeyLister(dir)
	opts := market.VerifyRecordOptions{
		KeysForDID:             keys,
		KeyResolver:            market.DefaultKeyResolver,
		KeyBoundByBadgeBlueKey: badge,
	}
	issuer := market.ATURIAuthority(event.Event.URI)
	if !market.VerifyRecordSignatures(ctx, event.Event.Value, issuer, opts) {
		return nil, fmt.Errorf("requester: event %s has no valid signature", event.Event.URI)
	}
	if !market.VerifyRecordSignatures(ctx, event.Payload.Value, issuer, opts) {
		return nil, fmt.Errorf("requester: payload %s has no valid signature", event.Payload.URI)
	}
	if payloadType := stringOf(event.Payload.Value["$type"]); payloadType != "" && payloadType != payloadNSID {
		return nil, fmt.Errorf("requester: payload %s names type %s", event.Payload.URI, payloadType)
	}
	out := &GuestEvent{Kind: payloadNSID, Event: event.Event, Payload: event.Payload}
	switch payloadNSID {
	case market.ComputeEventsVMOnNetworkNSID:
		address := stringOf(event.Payload.Value["address"])
		if _, err := ValidateTicket(address); err != nil {
			return nil, err
		}
		out.Address = address
		out.ObservedAt = stringOf(event.Payload.Value["observedAt"])
	case market.ComputeEventsVMHostKeysNSID:
		list, ok := event.Payload.Value["keys"].([]any)
		if !ok || len(list) == 0 {
			return nil, errors.New("requester: the host-keys event carries no keys")
		}
		raw := make([]v1alpha1.ComputeVMHostKey, 0, len(list))
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("requester: a host key is not an object")
			}
			raw = append(raw, v1alpha1.ComputeVMHostKey{
				Type:        stringOf(entry["type"]),
				PublicKey:   stringOf(entry["publicKey"]),
				Fingerprint: stringOf(entry["fingerprint"]),
			})
		}
		checked, err := computevm.CheckHostKeys(raw)
		if err != nil {
			return nil, err
		}
		out.HostKeys = checked
		out.ObservedAt = stringOf(event.Payload.Value["observedAt"])
	case market.ComputeEventsVMTerminatedNSID:
		reason := stringOf(event.Payload.Value["reason"])
		observedAt := stringOf(event.Payload.Value["observedAt"])
		if reason == "" || observedAt == "" {
			return nil, errors.New("requester: the termination event names no reason or time")
		}
		if _, err := market.ParseTime(observedAt); err != nil {
			return nil, fmt.Errorf("requester: termination time: %w", err)
		}
		termination := &v1alpha1.ContractTermination{
			Reason:     reason,
			ObservedAt: observedAt,
			Event:      commonv1.StrongRef{URI: event.Event.URI, CID: event.Event.CID},
			Payload:    commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID},
		}
		if code, ok := event.Payload.Value["exitCode"]; ok {
			value := int(numberValue(code))
			termination.ExitCode = &value
		}
		if request, ok := event.Payload.Value["request"].(map[string]any); ok {
			uri := stringOf(request["uri"])
			cid := stringOf(request["cid"])
			if uri != "" {
				termination.Request = &commonv1.StrongRef{URI: uri, CID: cid}
			}
		}
		out.Termination = termination
		out.ObservedAt = observedAt
	}
	if out.ObservedAt != "" {
		if _, err := market.ParseTime(out.ObservedAt); err != nil {
			return nil, fmt.Errorf("requester: observedAt: %w", err)
		}
	}
	out.Payload = market.ResolvedRecord{URI: ref.URI, CID: ref.CID, Value: event.Payload.Value}
	return out, nil
}

func sameRecordRef(a, b commonv1.StrongRef) bool {
	if a.URI == "" || b.URI == "" || a.URI != b.URI {
		return false
	}
	return a.CID == "" || b.CID == "" || a.CID == b.CID
}

func sameTermination(a, b *v1alpha1.ContractTermination) bool {
	if a == nil || b == nil {
		return false
	}
	return sameRecordRef(a.Event, b.Event) || sameRecordRef(a.Payload, b.Payload)
}

func terminationRecorded(state *v1alpha1.NodeClaimContract, termination *v1alpha1.ContractTermination) bool {
	if state == nil {
		return false
	}
	return sameTermination(state.Termination, termination)
}

func applyTermination(state *v1alpha1.NodeClaimContract, termination *v1alpha1.ContractTermination, now string) bool {
	if state == nil || termination == nil || terminationRecorded(state, termination) {
		return false
	}
	state.Termination = termination
	state.Outcome = v1alpha1.ContractOutcomeReturned
	state.Phase = v1alpha1.ContractReleased
	if state.Timestamps == nil {
		state.Timestamps = &v1alpha1.ContractTimestamps{}
	}
	state.Timestamps.Released = cmp.Or(state.Timestamps.Released, now)
	return true
}

func terminationMessage(termination *v1alpha1.ContractTermination) string {
	if termination == nil {
		return "the bidder ended the contract"
	}
	return "the bidder ended the contract: " + termination.Reason
}

func rememberGuestEvent(seen map[string]bool, uri string) {
	if uri != "" {
		seen[uri] = true
	}
}

func seedGuestEventSeen(seen map[string]bool, state *v1alpha1.NodeClaimContract) {
	if state == nil {
		return
	}
	if state.Termination != nil {
		rememberGuestEvent(seen, state.Termination.Event.URI)
		rememberGuestEvent(seen, state.Termination.Payload.URI)
	}
	if state.HostKeys != nil {
		rememberGuestEvent(seen, state.HostKeys.Event.URI)
		rememberGuestEvent(seen, state.HostKeys.Payload.URI)
	}
}

func guestEventSeen(seen map[string]bool, event MarketEvent) bool {
	keys := []string{event.Event.URI, event.Payload.URI}
	found := false
	for _, key := range keys {
		if key != "" && seen[key] {
			found = true
		}
	}
	for _, key := range keys {
		rememberGuestEvent(seen, key)
	}
	return found
}

func guestEventRecorded(eventRef, payloadRef commonv1.StrongRef, parsed *GuestEvent) bool {
	if parsed == nil {
		return false
	}
	return sameRecordRef(eventRef, commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID}) ||
		sameRecordRef(payloadRef, commonv1.StrongRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID})
}

func hostKeyLines(keys []v1alpha1.ComputeVMHostKey) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.Type+" "+key.PublicKey)
	}
	return out
}

func hostKeyAlias(receiptURI string) string {
	uri, err := market.ParseATURI(receiptURI)
	if err != nil {
		return "swc-guest"
	}
	clean := func(value string) string {
		var b strings.Builder
		for _, r := range value {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
				b.WriteRune(r)
			default:
				b.WriteRune('-')
			}
			if b.Len() >= 60 {
				break
			}
		}
		return strings.Trim(b.String(), "-")
	}
	alias := "swc-guest-" + clean(uri.Authority().String()) + "-" + clean(uri.RecordKey().String())
	if len(alias) > 128 {
		alias = alias[:128]
	}
	return alias
}

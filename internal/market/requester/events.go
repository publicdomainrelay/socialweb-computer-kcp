package requester

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
)

type GuestEvent struct {
	Kind string

	Event market.ResolvedRecord

	Payload market.ResolvedRecord

	Address string

	Tag string

	NetworkKind string

	GuestKind string

	Key string

	Ciphertext string

	SecretsVia string

	SecretsCount *int64

	HostKeys []v1alpha1.ComputeVMHostKey

	Termination *v1alpha1.ContractTermination

	Sandbox *v1alpha1.ContractSandbox

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

func isGuestEventNSID(nsid string) bool {
	switch nsid {
	case market.ComputeEventsVMOnNetworkNSID, market.ComputeEventsVMHostKeysNSID, market.ComputeEventsVMTerminatedNSID,
		market.ComputeEventsVMSandboxNSID, market.ComputeEventsGuestSecretsRequestedNSID, market.ComputeEventsGuestSecretsAppliedNSID:
		return true
	}
	return isContractEventNSID(nsid)
}

func isContractEventNSID(nsid string) bool {
	switch nsid {
	case market.ComputeEventsGuestPublicKeyNSID, market.ComputeEventsGuestSignalWebRTCNSID:
		return true
	}
	return false
}

func (q *Requester) badgeBlueBinding() func(ctx context.Context, did, key string) (bool, error) {
	return func(ctx context.Context, did, key string) (bool, error) {
		return market.KeyBoundByBadgeBlueKey(ctx, q.repo, did, key), nil
	}
}

func (q *Requester) verifyOptions() market.VerifyRecordOptions {
	return market.VerifyRecordOptions{
		KeysForDID:             market.NewDirectoryKeyLister(q.deps.Directory),
		KeyResolver:            market.DefaultKeyResolver,
		KeyBoundByBadgeBlueKey: q.badgeBlueBinding(),
	}
}

func (q *Requester) receiptOptions() market.VerifyRecordOptions {
	return market.VerifyRecordOptions{
		KeysForDID:  market.NewDirectoryKeyLister(q.deps.Directory),
		KeyResolver: market.DefaultKeyResolver,
	}
}

func (q *Requester) parseGuestEvent(ctx context.Context, event MarketEvent, receiptURI string) (*GuestEvent, error) {
	if event.Event.URI == "" || event.Payload.URI == "" {
		return nil, errors.New("requester: the market event has no event or payload record")
	}
	envelope, err := market.ParseEvent(event.Event.Value)
	if err != nil {
		return nil, err
	}
	payloadNSID := market.NSIDFromURI(event.Payload.URI)
	if !isGuestEventNSID(payloadNSID) {
		return nil, fmt.Errorf("requester: payload %s is not a guest event", payloadNSID)
	}
	ref, err := market.BindEventPayload(*envelope, event.Event.URI, receiptURI, payloadNSID)
	if err != nil {
		return nil, err
	}
	if market.ATURIAuthority(event.Event.URI) != market.ATURIAuthority(receiptURI) {
		return nil, fmt.Errorf("requester: event author is not the bidder")
	}
	opts := q.verifyOptions()
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
		tag := stringOf(event.Payload.Value["tag"])
		kind := market.OnNetworkKind(market.ComputeEventsVMOnNetwork{Kind: stringOf(event.Payload.Value["kind"]), Tag: tag})
		switch {
		case kind == market.OnNetworkKindIroh && tag == "":
			if _, err := ValidateTicket(address); err != nil {
				return nil, err
			}
			out.ObservedAt = stringOf(event.Payload.Value["observedAt"])
		case kind == market.OnNetworkKindIroh:
			return nil, fmt.Errorf("requester: the iroh address of %s names tag %q", event.Payload.URI, tag)
		case kind == market.OnNetworkKindSandbox && tag == "":
			if !strings.HasPrefix(address, "sandbox://") {
				return nil, fmt.Errorf("requester: payload %s names the sandbox address %q", event.Payload.URI, address)
			}
			out.ObservedAt = stringOf(event.Payload.Value["createdAt"])
		case tag == "":
			return nil, fmt.Errorf("requester: the %s address of %s names no tag", kind, event.Payload.URI)
		default:
			if err := ValidateNetworkAddress(kind, tag, address); err != nil {
				return nil, err
			}
			out.Tag = tag
			out.ObservedAt = stringOf(event.Payload.Value["createdAt"])
		}
		out.NetworkKind = kind
		out.Address = address
	case market.ComputeEventsGuestPublicKeyNSID, market.ComputeEventsGuestSignalWebRTCNSID:
		guest, err := parseGuestPayload(payloadNSID, event.Payload.Value)
		if err != nil {
			return nil, fmt.Errorf("requester: payload %s: %w", event.Payload.URI, err)
		}
		out.Tag, out.GuestKind, out.Key, out.Ciphertext = guest.Tag, guest.Kind, guest.Key, guest.Ciphertext
		out.ObservedAt = guest.CreatedAt
	case market.ComputeEventsGuestSecretsRequestedNSID:
		record, err := market.ParseComputeEventsGuestSecretsRequested(event.Payload.Value)
		if err != nil {
			return nil, fmt.Errorf("requester: payload %s: %w", event.Payload.URI, err)
		}
		out.ObservedAt = record.CreatedAt
	case market.ComputeEventsGuestSecretsAppliedNSID:
		record, err := market.ParseComputeEventsGuestSecretsApplied(event.Payload.Value)
		if err != nil {
			return nil, fmt.Errorf("requester: payload %s: %w", event.Payload.URI, err)
		}
		switch record.Via {
		case market.GuestSecretsViaGetSecrets, market.GuestSecretsViaSealed:
		default:
			return nil, fmt.Errorf("requester: payload %s names via %q", event.Payload.URI, record.Via)
		}
		out.SecretsVia = record.Via
		if _, ok := event.Payload.Value["count"]; ok && record.Count >= 0 {
			count := record.Count
			out.SecretsCount = &count
		}
		out.ObservedAt = record.CreatedAt
	case market.ComputeEventsVMHostKeysNSID:
		keys, err := q.reportedHostKeys(event.Payload.Value)
		if err != nil {
			return nil, err
		}
		out.HostKeys = keys
		out.ObservedAt = stringOf(event.Payload.Value["observedAt"])
	case market.ComputeEventsVMSandboxNSID:
		sandbox := &v1alpha1.ContractSandbox{ExitCode: int(numberValue(event.Payload.Value["exitCode"])),
			TimedOut: event.Payload.Value["timedOut"] == true, Stdout: stringOf(event.Payload.Value["stdout"]),
			Stderr: stringOf(event.Payload.Value["stderr"]), ObservedAt: stringOf(event.Payload.Value["createdAt"])}
		out.Sandbox = sandbox
		out.ObservedAt = sandbox.ObservedAt
	case market.ComputeEventsVMTerminatedNSID:
		termination, err := reportedTermination(event)
		if err != nil {
			return nil, err
		}
		out.Termination = termination
		out.ObservedAt = termination.ObservedAt
	}
	if out.ObservedAt != "" {
		if _, err := market.ParseTime(out.ObservedAt); err != nil {
			return nil, fmt.Errorf("requester: observedAt: %w", err)
		}
	}
	out.Payload = market.ResolvedRecord{URI: ref.URI, CID: ref.CID, Value: event.Payload.Value}
	return out, nil
}

func (q *Requester) reportedHostKeys(value map[string]any) ([]v1alpha1.ComputeVMHostKey, error) {
	list, ok := value["keys"].([]any)
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
	return q.deps.Guests.CheckHostKeys(raw)
}

func reportedTermination(event MarketEvent) (*v1alpha1.ContractTermination, error) {
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
	return termination, nil
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
	for _, n := range state.Networks {
		rememberGuestEvent(seen, n.Event.URI)
	}
	if state.SecretsRequested != nil {
		rememberGuestEvent(seen, state.SecretsRequested.Event.URI)
		rememberGuestEvent(seen, state.SecretsRequested.Payload.URI)
	}
	if state.SecretsApplied != nil {
		rememberGuestEvent(seen, state.SecretsApplied.Event.URI)
		rememberGuestEvent(seen, state.SecretsApplied.Payload.URI)
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

func guestEventApplied(state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	if state == nil || parsed == nil {
		return false
	}
	switch parsed.Kind {
	case market.ComputeEventsVMOnNetworkNSID:
		if parsed.Tag != "" {
			return networkRecorded(state, parsed)
		}
		return state.VMAddress != ""
	case market.ComputeEventsVMHostKeysNSID:
		return state.HostKeys != nil && guestEventRecorded(state.HostKeys.Event, state.HostKeys.Payload, parsed)
	case market.ComputeEventsVMTerminatedNSID:
		return terminationRecorded(state, parsed.Termination)
	case market.ComputeEventsGuestSecretsRequestedNSID:
		return state.SecretsRequested != nil && guestEventRecorded(state.SecretsRequested.Event, state.SecretsRequested.Payload, parsed)
	case market.ComputeEventsGuestSecretsAppliedNSID:
		return state.SecretsApplied != nil && guestEventRecorded(state.SecretsApplied.Event, state.SecretsApplied.Payload, parsed)
	}
	return false
}

func laterTime(a, b string) bool {
	ta, errA := market.ParseTime(a)
	tb, errB := market.ParseTime(b)
	if errA != nil || errB != nil {
		return a > b
	}
	return ta.After(tb)
}

func applySecretsEvent(state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	event := commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID}
	payload := commonv1.StrongRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID}
	switch parsed.Kind {
	case market.ComputeEventsGuestSecretsRequestedNSID:
		if prev := state.SecretsRequested; prev != nil && (guestEventRecorded(prev.Event, prev.Payload, parsed) || laterTime(prev.RequestedAt, parsed.ObservedAt)) {
			return false
		}
		state.SecretsRequested = &v1alpha1.ContractSecretsRequested{RequestedAt: parsed.ObservedAt, Event: event, Payload: payload}
		return true
	case market.ComputeEventsGuestSecretsAppliedNSID:
		if prev := state.SecretsApplied; prev != nil && (guestEventRecorded(prev.Event, prev.Payload, parsed) || laterTime(prev.AppliedAt, parsed.ObservedAt)) {
			return false
		}
		state.SecretsApplied = &v1alpha1.ContractSecretsApplied{AppliedAt: parsed.ObservedAt, Via: parsed.SecretsVia, Count: parsed.SecretsCount,
			Event: event, Payload: payload}
		return true
	}
	return false
}

func secretsRequestPending(state *v1alpha1.NodeClaimContract) bool {
	if state == nil || state.SecretsRequested == nil {
		return false
	}
	applied := state.SecretsApplied
	return applied == nil || laterTime(state.SecretsRequested.RequestedAt, applied.AppliedAt)
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

func sandboxAddress(address string) bool {
	return strings.HasPrefix(address, "sandbox://")
}

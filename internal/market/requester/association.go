package requester

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
)

const (
	ContractAssociationLifetime = market.ContractAssociationLifetime

	// ContractExpiresAtLabel on a ComputeContract is the unix time (seconds) its
	// compute.vm asks the bidder to end the VM at (compute.vm expiresAt).
	ContractExpiresAtLabel = "socialweb.computer/vm-expires-at"

	associationRetryFirst = 2 * time.Second

	associationRetryMax = 5 * time.Minute

	associationRetryAttempts = 12

	associationNonceBytes = 24

	associationStepTimeout = 30 * time.Second
)

var errAssociationRefused = errors.New("requester: the association is refused")

type contractAssociation struct {
	subject string

	rkey string

	ref commonv1.StrongRef

	expiresAt time.Time

	finished bool
}

func associationSubject(contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract) string {
	a := contract.Spec.Association
	if a == nil {
		return ""
	}
	subject := strings.TrimSpace(a.Subject)
	switch {
	case a.FromGuest && subject != "":
		return ""
	case subject != "":
		return subject
	case a.FromGuest && state != nil && state.GuestRequester != nil && state.GuestRequester.State == v1alpha1.ContractGuestRequesterVerified:
		return strings.TrimSpace(state.GuestRequester.Did)
	}
	return ""
}

func fromGuest(contract v1alpha1.ComputeContract) bool {
	a := contract.Spec.Association
	return a != nil && a.FromGuest && strings.TrimSpace(a.Subject) == ""
}

func associationRefFile(contract v1alpha1.ComputeContract) (string, error) {
	if contract.Spec.Association == nil {
		return cloudinit.DefaultAssociationRefFile, nil
	}
	return cloudinit.AssociationRefPath(contract.Spec.Association.RefFile)
}

func (q *Requester) endorsementRefused(ctx context.Context) error {
	granted, known, err := q.oauthGrants(ctx)
	if err != nil || !known || !atprotooauth.KnownGrant(granted) || len(atprotooauth.MissingScopes([]string{atprotooauth.RepoEndorsementWrite}, granted)) == 0 {
		return nil
	}
	q.log.Warn("requester_endorsement_skipped", "hint", "the oauth session was not granted the optional scope "+atprotooauth.RepoEndorsementWrite+
		"; sign in again to grant it; the agent stays unassociated until then")
	return fmt.Errorf("%w: the oauth session was not granted %s", errAssociationRefused, atprotooauth.RepoEndorsementWrite)
}

func (r *contractRunner) associationWritten(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.association != nil && r.association.subject == subject
}

func (r *contractRunner) associationFinished(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.association != nil && r.association.subject == subject && r.association.finished
}

func guestPending(state *v1alpha1.NodeClaimContract) bool {
	return state != nil && state.GuestRequester != nil && state.GuestRequester.State == v1alpha1.ContractGuestRequesterPending
}

func (r *contractRunner) associationPending(contract v1alpha1.ComputeContract) bool {
	state := contract.Status.Contract
	if state == nil || state.Accept == nil || state.Accept.URI == "" || contract.Spec.Release || contract.DeletionTimestamp != nil {
		return false
	}
	if fromGuest(contract) && guestPending(state) {
		return true
	}
	subject := associationSubject(contract, state)
	return subject != "" && !r.associationFinished(subject)
}

func (r *contractRunner) associateLater(contract v1alpha1.ComputeContract) {
	if r.associationPending(contract) {
		r.kickAssociation()
	}
}

func (r *contractRunner) kickAssociation() {
	select {
	case r.associationKick <- struct{}{}:
	default:
	}
}

func (r *contractRunner) associationStep(ctx context.Context, state *v1alpha1.NodeClaimContract) {
	if state == nil || r.isGone() {
		return
	}
	stepCtx, cancel := context.WithTimeout(ctx, associationStepTimeout)
	defer cancel()
	r.ensureAssociation(stepCtx, state.Accept, state)
}

func (r *contractRunner) associationFailed(ctx context.Context, step string, err error) bool {
	refused := errors.Is(err, errAssociationRefused) || errors.Is(err, errAtircLeaseMissing)
	r.log().Warn("requester_association_failed", "step", step, "err", err.Error(), "retry", !refused)
	message := step + ": " + err.Error()
	r.mu.Lock()
	attempts := r.associationAttempts
	if !refused {
		r.associationAttempts++
	}
	r.mu.Unlock()
	terminal := refused || attempts >= associationRetryAttempts
	switch {
	case refused:
	case terminal:
		message += fmt.Sprintf(" (gave up after %d attempts)", attempts)
	default:
		delay := min(associationRetryFirst<<attempts, associationRetryMax)
		message += " (retry in " + delay.String() + ")"
		r.mu.Lock()
		if r.associationRetry != nil {
			r.associationRetry.Stop()
		}
		r.associationRetry = time.AfterFunc(delay, r.kickAssociation)
		r.mu.Unlock()
	}
	if err := r.q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{AssociationMessage: message}); err != nil && !notFound(err) {
		r.log().Warn("requester_association_status_failed", "err", err.Error())
	}
	return terminal
}

func associationTimes(contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract) (time.Time, error) {
	if state != nil && state.Timestamps != nil && state.Timestamps.Accepted != "" {
		return time.Parse(time.RFC3339Nano, state.Timestamps.Accepted)
	}
	if contract.CreationTimestamp.IsZero() {
		return time.Time{}, errors.New("the contract has no accept time and no creation time")
	}
	return contract.CreationTimestamp.UTC(), nil
}

func (r *contractRunner) ensureAssociation(ctx context.Context, accept *commonv1.StrongRef, state *v1alpha1.NodeClaimContract) {
	contract := r.snapshot()
	if state == nil {
		state = r.stateCopy()
	}
	if accept == nil || accept.URI == "" || accept.CID == "" || contract.Spec.Association == nil {
		return
	}
	if fromGuest(contract) {
		if err := r.ensureAssociationRequest(ctx, contract, *accept); err != nil {
			r.associationFailed(ctx, "the association request entry", err)
			return
		}
		if guestPending(state) {
			r.proveGuestRequester(ctx, state)
			return
		}
	}
	subject := associationSubject(contract, state)
	if subject == "" || r.associationFinished(subject) {
		return
	}
	r.associationMu.Lock()
	defer r.associationMu.Unlock()
	if r.associationFinished(subject) {
		return
	}
	if !r.associationWritten(subject) {
		if step, err := r.associate(ctx, contract, *accept, state, subject); err != nil {
			r.associationFailed(ctx, step, err)
			return
		}
	}
	if step, err := r.finishAssociation(ctx, contract, state); err != nil {
		r.associationFailed(ctx, step, err)
		return
	}
	r.mu.Lock()
	r.associationAttempts = 0
	r.mu.Unlock()
}

func (r *contractRunner) finishAssociation(ctx context.Context, contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract) (string, error) {
	r.mu.Lock()
	current := r.association
	r.mu.Unlock()
	if current == nil {
		return "", nil
	}
	if state != nil && state.Accept != nil {
		if err := r.ensureGuestRequesterToken(ctx, contract, state, current.ref, current.expiresAt); err != nil {
			return "mint the guest requester's atirc token", err
		}
	}
	if contract.Spec.SecretsRef != nil && (fromGuest(contract) || r.secretsFetched() != "" || contract.Status.SecretsFetchedAt != "" ||
		(state != nil && state.SecretsApplied != nil)) {
		if err := r.notifySecretsUpdated(ctx, contract, current.ref); err != nil {
			return "notify the guest", err
		}
	}
	r.mu.Lock()
	if r.association == current {
		current.finished = true
	}
	r.mu.Unlock()
	return "", nil
}

func (r *contractRunner) associate(ctx context.Context, contract v1alpha1.ComputeContract, accept commonv1.StrongRef, state *v1alpha1.NodeClaimContract, subject string) (string, error) {
	q := r.q
	if did, err := syntax.ParseDID(subject); err != nil || did.String() != subject || subject == q.repoDID {
		return "subject", fmt.Errorf("%w: spec.association.subject %q is not a DID other than the requester's", errAssociationRefused, subject)
	}
	refFile, err := associationRefFile(contract)
	if err != nil {
		return "refFile", fmt.Errorf("%w: %v", errAssociationRefused, err)
	}
	acceptedAt, err := associationTimes(contract, state)
	if err != nil {
		return "deadline", fmt.Errorf("%w: %v", errAssociationRefused, err)
	}
	expiresAt := acceptedAt.Add(ContractAssociationLifetime)
	if !q.now().Before(expiresAt) {
		return "deadline", fmt.Errorf("%w: the contract deadline %s has passed", errAssociationRefused, market.FormatTime(expiresAt))
	}
	if err := q.endorsementRefused(ctx); err != nil {
		return "the endorsement scope", err
	}
	rkey := subject
	existing, err := q.repo.GetRecord(ctx, q.repoDID, market.AssociationNSID, rkey)
	switch {
	case err == nil:
		if bound, ok := recordContract(existing.Value); !ok || bound != accept {
			return "write the grant", fmt.Errorf("%w: %s is bound to another contract", errAssociationRefused, existing.URI)
		}
	case !errors.Is(err, market.ErrNotFound):
		return "read the grant", err
	}
	r.mu.Lock()
	previous := r.association
	r.mu.Unlock()
	record := market.ContractGrantRecord(subject, map[string]any{"uri": accept.URI, "cid": accept.CID}, market.FormatTime(expiresAt), market.FormatTime(acceptedAt))
	written, err := q.repo.PutRecord(ctx, q.repoDID, market.AssociationNSID, rkey, record)
	if err != nil {
		return "write the grant", err
	}
	if _, err := q.repo.PutRecord(ctx, q.repoDID, market.EndorsementNSID, rkey, market.EndorsementRecord(subject)); err != nil {
		return "write the endorsement", err
	}
	r.mu.Lock()
	if r.associationRecords == nil {
		r.associationRecords = map[string]string{}
	}
	r.associationRecords[rkey] = subject
	r.mu.Unlock()
	ref := commonv1.StrongRef{URI: written.URI, CID: written.CID}
	if contract.Spec.SecretsRef != nil {
		entry := SecretEntry{Path: refFile, Value: ref.URI + "#" + ref.CID + "\n", Mode: "0600"}
		if err := r.putBundleEntry(ctx, contract, entry); err != nil {
			return "write the bundle entry", err
		}
	}
	status := v1alpha1.ComputeContractStatus{Association: &ref, AssociationMessage: "the grant " + ref.URI + " and its endorsement name " + subject}
	if err := q.deps.Intents.MergeComputeContractStatus(ctx, r.name, status); err != nil && !notFound(err) {
		return "write the contract status", err
	}
	r.mu.Lock()
	r.association = &contractAssociation{subject: subject, rkey: rkey, ref: ref, expiresAt: expiresAt}
	r.mu.Unlock()
	if previous != nil && previous.rkey != rkey {
		r.deleteAssociationRecord(ctx, previous.rkey, accept)
	}
	r.log().Info("requester_association_written", "subject", subject, "uri", ref.URI, "cid", ref.CID, "expiresAt", market.FormatTime(expiresAt))
	return "", nil
}

func recordContract(value map[string]any) (commonv1.StrongRef, bool) {
	fields, ok := value["contract"].(map[string]any)
	if !ok {
		return commonv1.StrongRef{}, false
	}
	uri, _ := fields["uri"].(string)
	cid, _ := fields["cid"].(string)
	return commonv1.StrongRef{URI: uri, CID: cid}, uri != "" && cid != ""
}

func (r *contractRunner) putBundleEntry(ctx context.Context, contract v1alpha1.ComputeContract, entry SecretEntry) error {
	if tokenFile(contract, entry.Path) {
		return fmt.Errorf("%w: %s", ErrTokenPathConflict, entry.Path)
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	bundleRef := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, bundleRef)
	if err != nil {
		return err
	}
	replaced := false
	for i, e := range entries {
		if e.Path == entry.Path {
			if e == entry {
				r.addSecretEntries(entry)
				return nil
			}
			entries[i], replaced = entry, true
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, bundleRef.Mount, bundleRef.Path, next); err != nil {
		return err
	}
	r.addSecretEntries(entry)
	return nil
}

func (r *contractRunner) bundleEntry(ctx context.Context, contract v1alpha1.ComputeContract, path string) (SecretEntry, bool, error) {
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	_, _, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		return SecretEntry{}, false, err
	}
	for _, e := range entries {
		if e.Path == path {
			return e, true, nil
		}
	}
	return SecretEntry{}, false, nil
}

func (r *contractRunner) associationRequest(ctx context.Context, contract v1alpha1.ComputeContract, accept commonv1.StrongRef) (v1alpha1.AssociationRequest, bool, error) {
	entry, found, err := r.bundleEntry(ctx, contract, cloudinit.DefaultAssociationRequestFile)
	if err != nil || !found {
		return v1alpha1.AssociationRequest{}, false, err
	}
	var request v1alpha1.AssociationRequest
	if err := json.Unmarshal([]byte(entry.Value), &request); err != nil {
		return v1alpha1.AssociationRequest{}, false, nil
	}
	ok := request.Principal == r.q.repoDID && request.Contract == accept.URI && len(request.Nonce) == 2*associationNonceBytes
	return request, ok, nil
}

func (r *contractRunner) ensureAssociationRequest(ctx context.Context, contract v1alpha1.ComputeContract, accept commonv1.StrongRef) error {
	if contract.Spec.SecretsRef == nil {
		return fmt.Errorf("%w: spec.association.fromGuest needs spec.secretsRef", errAssociationRefused)
	}
	if _, ok, err := r.associationRequest(ctx, contract, accept); err != nil || ok {
		return err
	}
	nonce := make([]byte, associationNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	raw, err := json.Marshal(v1alpha1.AssociationRequest{Principal: r.q.repoDID, Contract: accept.URI, Nonce: hex.EncodeToString(nonce),
		RotationKey: r.q.renterRotationDID()})
	if err != nil {
		return err
	}
	if err := r.putBundleEntry(ctx, contract, SecretEntry{Path: cloudinit.DefaultAssociationRequestFile, Value: string(raw) + "\n", Mode: "0600"}); err != nil {
		return err
	}
	r.log().Info("requester_association_request_entry", "path", cloudinit.DefaultAssociationRequestFile, "contract", accept.URI)
	return nil
}

func (r *contractRunner) addSecretEntries(entries ...SecretEntry) {
	r.mu.Lock()
	state := r.secrets
	r.mu.Unlock()
	if state != nil {
		state.addEntries(entries...)
	}
}

func (r *contractRunner) deleteAssociation(ctx context.Context) {
	state := r.stateCopy()
	if state == nil || state.Accept == nil || state.Accept.URI == "" {
		return
	}
	accept := *state.Accept
	rkeys := map[string]bool{}
	if subject := associationSubject(r.snapshot(), state); subject != "" {
		rkeys[subject] = true
	}
	r.mu.Lock()
	if r.association != nil {
		rkeys[r.association.rkey] = true
	}
	for rkey := range r.associationRecords {
		rkeys[rkey] = true
	}
	if r.associationRetry != nil {
		r.associationRetry.Stop()
	}
	r.mu.Unlock()
	for rkey := range rkeys {
		r.deleteAssociationRecord(ctx, rkey, accept)
	}
	r.tombstoneFallback(state)
}

func (r *contractRunner) deleteAssociationRecord(ctx context.Context, rkey string, accept commonv1.StrongRef) {
	q := r.q
	if q.repo == nil {
		return
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	forget := func() {
		r.mu.Lock()
		if r.association != nil && r.association.rkey == rkey {
			r.association = nil
		}
		delete(r.associationRecords, rkey)
		r.mu.Unlock()
	}
	existing, err := q.repo.GetRecord(deleteCtx, q.repoDID, market.AssociationNSID, rkey)
	switch {
	case errors.Is(err, market.ErrNotFound):
	case err != nil:
		r.log().Warn("requester_association_delete_failed", "rkey", rkey, "step", "read", "err", err.Error())
		return
	default:
		if bound, ok := recordContract(existing.Value); !ok || bound != accept {
			r.log().Warn("requester_association_delete_skipped", "rkey", rkey, "reason", "the grant is bound to another contract")
			forget()
			return
		}
	}
	for _, collection := range []string{market.EndorsementNSID, market.AssociationNSID} {
		if err := q.repo.DeleteRecord(deleteCtx, q.repoDID, collection, rkey); err != nil && !errors.Is(err, market.ErrNotFound) {
			r.log().Warn("requester_association_delete_failed", "rkey", rkey, "collection", collection, "err", err.Error())
			return
		}
	}
	forget()
	r.log().Info("requester_association_deleted", "rkey", rkey)
}

func (r *contractRunner) deleteContractRecords(ctx context.Context) {
	r.deleteRBAC(ctx)
	r.deleteAssociation(ctx)
	r.deleteMarketRecords(ctx)
}

func (r *contractRunner) deleteMarketRecords(ctx context.Context) {
	q := r.q
	if q.repo == nil {
		return
	}
	var refs []commonv1.StrongRef
	if state := r.stateCopy(); state != nil {
		if state.Release != nil {
			refs = append(refs, state.Release.Event, state.Release.Payload)
		}
		for _, ref := range []*commonv1.StrongRef{state.Accept, state.RFP, state.Payload, state.Policy} {
			if ref != nil {
				refs = append(refs, *ref)
			}
		}
	}
	for _, event := range q.contractEvents.Filter(func(e v1alpha1.ContractEvent) bool {
		return e.Spec.ContractRef.Name == r.name && e.Spec.Direction == v1alpha1.ContractEventOutbound
	}) {
		for _, ref := range []*commonv1.StrongRef{event.Status.Event, event.Status.Record} {
			if ref != nil {
				refs = append(refs, *ref)
			}
		}
		refs = append(refs, q.contractEventRefs(event)...)
	}
	refs = uniqueRefs(refs)
	deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deleted := 0
	var unscoped []string
	for _, ref := range refs {
		aturi, err := syntax.ParseATURI(ref.URI)
		if err != nil || aturi.Authority().String() != q.repoDID {
			continue
		}
		if err := q.repo.DeleteRecord(deleteCtx, q.repoDID, aturi.Collection().String(), aturi.RecordKey().String()); err != nil && !errors.Is(err, market.ErrNotFound) {
			if strings.Contains(err.Error(), "ScopeMissingError") {
				unscoped = append(unscoped, aturi.Collection().String())
				continue
			}
			r.log().Warn("requester_record_delete_failed", "record", ref.URI, "err", err)
			continue
		}
		deleted++
	}
	if len(unscoped) > 0 {
		r.log().Info("requester_record_delete_skipped", "reason", "the session was not granted delete on these collections; sign in again to grant it",
			"collections", slices.Compact(slices.Sorted(slices.Values(unscoped))))
	}
	r.log().Info("requester_contract_records_deleted", "deleted", deleted, "of", len(refs))
}

// The payload and market.event an outbound ContractEvent was published at (publishContractEvent's rkeys, every
// generation up to its current one): found even when its status never recorded them (the invocation that published
// it stopped first).
func (q *Requester) contractEventRefs(event v1alpha1.ContractEvent) []commonv1.StrongRef {
	if event.UID == "" {
		return nil
	}
	var out []commonv1.StrongRef
	for g := int64(1); g <= max(event.Generation, 1); g++ {
		uid := string(event.UID)
		if g > 1 {
			uid += "-g" + strconv.FormatInt(g, 10)
		}
		if nsid := event.Spec.NSID; nsid != "" {
			out = append(out, commonv1.StrongRef{URI: "at://" + q.repoDID + "/" + nsid + "/" + sessionRKey("contract-event", uid)})
		}
		out = append(out, commonv1.StrongRef{URI: "at://" + q.repoDID + "/" + market.EventNSID + "/" + sessionRKey("contract-event-envelope", uid)})
	}
	return out
}

func uniqueRefs(in []commonv1.StrongRef) []commonv1.StrongRef {
	seen := map[string]bool{}
	out := in[:0]
	for _, ref := range in {
		if !seen[ref.URI] {
			seen[ref.URI] = true
			out = append(out, ref)
		}
	}
	return out
}

func SecretsUpdatedEventName(contract string, ref commonv1.StrongRef) string {
	return "secrets-updated-" + market.SessionRKey("secretsUpdated", contract, ref.URI, ref.CID)
}

func (r *contractRunner) notifySecretsUpdated(ctx context.Context, contract v1alpha1.ComputeContract, ref commonv1.StrongRef) error {
	q := r.q
	if q.deps.ContractEvents == nil {
		return errors.New("the requester has no ContractEvents handle")
	}
	if contract.UID == "" {
		return errors.New("the contract has no uid yet")
	}
	name := SecretsUpdatedEventName(contract.Name, ref)
	if _, known := q.contractEvents.Get(name); known {
		return nil
	}
	event := v1alpha1.ContractEvent{
		ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{contractOwnerReference(contract)}},
		Spec: v1alpha1.ContractEventSpec{
			ContractRef: commonv1.LocalObjectRef{Name: contract.Name},
			Direction:   v1alpha1.ContractEventOutbound,
			NSID:        market.ComputeEventsGuestSecretsUpdatedNSID,
		},
	}
	if err := q.deps.ContractEvents.ApplyContractEvent(ctx, event); err != nil {
		return err
	}
	q.contractEvents.Put(name, event)
	r.log().Info("requester_secrets_updated_sent", "contractEvent", name, "association", ref.URI)
	return nil
}

func (r *contractRunner) onGuestRequester(ctx context.Context, state *v1alpha1.NodeClaimContract, parsed *GuestEvent) {
	contract := r.snapshot()
	refuse := func(reason string) {
		r.log().Warn("requester_guest_requester_refused", "did", parsed.RequesterDID, "event", parsed.Event.URI, "reason", reason)
	}
	switch {
	case !fromGuest(contract):
		refuse("the contract has no spec.association.fromGuest")
		return
	case contract.Spec.Release || contract.DeletionTimestamp != nil:
		refuse("the contract is releasing")
		return
	}
	if r.recordGuestRequester(ctx, state, parsed) {
		r.proveGuestRequester(ctx, state)
	}
}

func (r *contractRunner) recordGuestRequester(ctx context.Context, state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	refuse := func(reason string) bool {
		r.log().Warn("requester_guest_requester_refused", "did", parsed.RequesterDID, "event", parsed.Event.URI, "reason", reason)
		return false
	}
	switch {
	case !fromGuest(r.snapshot()):
		return refuse("the contract has no spec.association.fromGuest")
	case state.GuestRequester != nil && state.GuestRequester.Did == parsed.RequesterDID:
		return false
	case state.GuestRequester != nil:
		return refuse("the guest already reported " + state.GuestRequester.Did)
	}
	if err := market.ValidGuestRequesterDID(parsed.RequesterDID, r.q.repoDID); err != nil {
		return refuse(err.Error())
	}
	state.GuestRequester = &v1alpha1.ContractGuestRequester{Did: parsed.RequesterDID, State: v1alpha1.ContractGuestRequesterPending,
		Message: "proving control of the reported DID", ReportedAt: parsed.ObservedAt,
		Event:   commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID},
		Payload: commonv1.StrongRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID}}
	if err := r.writeGuestRequester(ctx, state); err != nil {
		state.GuestRequester = nil
		return refuse("recording the reported DID failed: " + err.Error())
	}
	r.mu.Lock()
	r.associationAttempts = 0
	r.mu.Unlock()
	r.log().Info("requester_guest_requester_reported", "did", parsed.RequesterDID)
	return true
}

func (r *contractRunner) writeGuestRequester(ctx context.Context, state *v1alpha1.NodeClaimContract) error {
	contract := r.snapshot()
	phase, message := contract.Status.Phase, contract.Status.Message
	if phase == "" {
		phase = v1alpha1.ComputeContractHeld
	}
	return r.writeStatus(ctx, phase, message, state, r.currentBids(), r.currentChosen(), "associating", 1)
}

func (r *contractRunner) settleGuestRequester(ctx context.Context, state *v1alpha1.NodeClaimContract, to v1alpha1.ContractGuestRequesterState, message string) error {
	previous := *state.GuestRequester
	state.GuestRequester.State, state.GuestRequester.Message = to, message
	if err := r.writeGuestRequester(ctx, state); err != nil {
		*state.GuestRequester = previous
		return err
	}
	return nil
}

func (r *contractRunner) proveGuestRequester(ctx context.Context, state *v1alpha1.NodeClaimContract) {
	q := r.q
	contract := r.snapshot()
	if !guestPending(state) || state.Accept == nil || state.Accept.URI == "" {
		return
	}
	accept := *state.Accept
	did := state.GuestRequester.Did
	fail := func(step string, err error) {
		if !r.associationFailed(ctx, step, err) {
			return
		}
		if err := r.settleGuestRequester(ctx, state, v1alpha1.ContractGuestRequesterRefused, step+": "+err.Error()); err != nil {
			r.log().Warn("requester_guest_requester_status_failed", "err", err.Error())
		}
	}
	refuse := func(reason string) {
		fail("the guest requester's association request", fmt.Errorf("%w: %s", errAssociationRefused, reason))
	}
	if err := market.ValidGuestRequesterDID(did, q.repoDID); err != nil {
		refuse(err.Error())
		return
	}
	request, ok, err := r.associationRequest(ctx, contract, accept)
	switch {
	case err != nil:
		fail("the association request entry", err)
		return
	case !ok:
		refuse("the contract's bundle holds no association request for " + accept.URI)
		return
	}
	rkey := q.repoDID
	got, err := q.repo.GetRecord(ctx, did, market.AssociationRequestNSID, rkey)
	if err != nil {
		fail("reading "+did+"'s "+market.AssociationRequestNSID+"/"+rkey, err)
		return
	}
	asked, err := market.ParseAssociationRequest(got.Value)
	switch {
	case err != nil:
		refuse("the association request record is invalid: " + err.Error())
		return
	case market.ATURIAuthority(got.URI) != did:
		refuse("the association request record is not in " + did + "'s repo")
		return
	case asked.Principal != q.repoDID || asked.Contract != accept.URI:
		refuse("the association request record names another principal or contract")
		return
	case subtle.ConstantTimeCompare([]byte(asked.Nonce), []byte(request.Nonce)) != 1:
		refuse("the association request record carries another nonce")
		return
	}
	state.GuestRequester.Accounts = guestAccounts(asked.Accounts, q.repoDID, did)
	if err := r.settleGuestRequester(ctx, state, v1alpha1.ContractGuestRequesterVerified, "the association request "+got.URI+" matches"); err != nil {
		fail("record the verified guest requester", err)
		return
	}
	r.log().Info("requester_guest_requester", "did", did, "request", got.URI, "accounts", state.GuestRequester.Accounts)
	r.ensureAssociation(ctx, state.Accept, state)
}

func contractExpiresAt(label string) string {
	sec, err := strconv.ParseInt(strings.TrimSpace(label), 10, 64)
	if err != nil || sec <= 0 {
		return ""
	}
	return market.FormatTime(time.Unix(sec, 0))
}

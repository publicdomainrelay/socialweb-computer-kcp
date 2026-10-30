package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const ContractAssociationLifetime = 24 * time.Hour

type contractAssociation struct {
	subject string

	ref commonv1.StrongRef
}

func associationSubject(contract v1alpha1.ComputeContract) string {
	if contract.Spec.Association == nil {
		return ""
	}
	return strings.TrimSpace(contract.Spec.Association.Subject)
}

func associationRefFile(contract v1alpha1.ComputeContract) (string, error) {
	if contract.Spec.Association == nil {
		return cloudinit.DefaultAssociationRefFile, nil
	}
	return cloudinit.AssociationRefPath(contract.Spec.Association.RefFile)
}

func (r *contractRunner) associationWritten(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.association != nil && r.association.subject == subject
}

func (r *contractRunner) associationPending(contract v1alpha1.ComputeContract) bool {
	subject := associationSubject(contract)
	state := contract.Status.Contract
	return subject != "" && state != nil && state.Accept != nil && state.Accept.URI != "" && !contract.Spec.Release &&
		contract.DeletionTimestamp == nil && !r.associationWritten(subject)
}

func (r *contractRunner) associateLater(contract v1alpha1.ComputeContract) {
	if !r.associationPending(contract) || !r.associating.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer r.associating.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), atircCallTimeout)
		defer cancel()
		r.ensureAssociation(ctx, contract.Status.Contract.Accept, contract.Status.Contract)
	}()
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
	q := r.q
	contract := r.snapshot()
	subject := associationSubject(contract)
	if subject == "" || accept == nil || accept.URI == "" || accept.CID == "" || r.associationWritten(subject) {
		return
	}
	r.associationMu.Lock()
	defer r.associationMu.Unlock()
	if r.associationWritten(subject) {
		return
	}
	fail := func(step string, err error) {
		r.log().Warn("requester_association_failed", "subject", subject, "step", step, "err", err.Error())
	}
	if did, err := syntax.ParseDID(subject); err != nil || did.String() != subject || subject == q.repoDID {
		fail("subject", fmt.Errorf("spec.association.subject %q is not a DID other than the requester's", subject))
		return
	}
	refFile, err := associationRefFile(contract)
	if err != nil {
		fail("refFile", err)
		return
	}
	acceptedAt, err := associationTimes(contract, state)
	if err != nil {
		fail("deadline", err)
		return
	}
	expiresAt := acceptedAt.Add(ContractAssociationLifetime)
	if !q.now().Before(expiresAt) {
		fail("deadline", fmt.Errorf("the contract deadline %s has passed", market.FormatTime(expiresAt)))
		return
	}
	r.mu.Lock()
	previous := r.association
	r.mu.Unlock()
	record := map[string]any{
		"$type":     market.AssociationNSID,
		"subject":   subject,
		"service":   trust.RequesterAssociate,
		"contract":  map[string]any{"uri": accept.URI, "cid": accept.CID},
		"expiresAt": market.FormatTime(expiresAt),
		"createdAt": market.FormatTime(acceptedAt),
	}
	rkey := trust.ClaimRKey(trust.RequesterAssociate, subject)
	written, err := q.repo.PutRecord(ctx, q.repoDID, market.AssociationNSID, rkey, record)
	if err != nil {
		fail("write the market.association record", err)
		return
	}
	ref := commonv1.StrongRef{URI: written.URI, CID: written.CID}
	if contract.Spec.SecretsRef != nil {
		if err := r.writeAssociationRef(ctx, contract, refFile, ref); err != nil {
			fail("write the bundle entry", err)
			return
		}
	}
	if err := q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{Association: &ref}); err != nil && !notFound(err) {
		fail("write the contract status", err)
		return
	}
	r.mu.Lock()
	r.association = &contractAssociation{subject: subject, ref: ref}
	r.mu.Unlock()
	if previous != nil && previous.subject != subject {
		r.deleteAssociationRecord(ctx, previous.subject)
	}
	r.log().Info("requester_association_written", "subject", subject, "uri", ref.URI, "cid", ref.CID, "expiresAt", market.FormatTime(expiresAt))
}

func (r *contractRunner) writeAssociationRef(ctx context.Context, contract v1alpha1.ComputeContract, path string, ref commonv1.StrongRef) error {
	bundleRef := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, bundleRef)
	if err != nil {
		return err
	}
	entry := SecretEntry{Path: path, Value: ref.URI + "#" + ref.CID + "\n", Mode: "0600"}
	replaced := false
	for i, e := range entries {
		if e.Path == path {
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

func (r *contractRunner) addSecretEntries(entries ...SecretEntry) {
	r.mu.Lock()
	state := r.secrets
	r.mu.Unlock()
	if state != nil {
		state.addEntries(entries...)
	}
}

func (r *contractRunner) deleteAssociation(ctx context.Context) {
	subject := associationSubject(r.snapshot())
	r.mu.Lock()
	if r.association != nil {
		subject = r.association.subject
	}
	r.mu.Unlock()
	if subject == "" {
		return
	}
	r.deleteAssociationRecord(ctx, subject)
}

func (r *contractRunner) deleteAssociationRecord(ctx context.Context, subject string) {
	if r.q.repo == nil {
		return
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	rkey := trust.ClaimRKey(trust.RequesterAssociate, subject)
	if err := r.q.repo.DeleteRecord(deleteCtx, r.q.repoDID, market.AssociationNSID, rkey); err != nil && !errors.Is(err, market.ErrNotFound) && !strings.Contains(err.Error(), "not found") {
		r.log().Warn("requester_association_delete_failed", "subject", subject, "rkey", rkey, "err", err.Error())
		return
	}
	r.mu.Lock()
	if r.association != nil && r.association.subject == subject {
		r.association = nil
	}
	r.mu.Unlock()
	r.log().Info("requester_association_deleted", "subject", subject, "rkey", rkey)
}

func (r *contractRunner) deleteContractRecords(ctx context.Context) {
	r.deleteRBAC(ctx)
	r.deleteAssociation(ctx)
}

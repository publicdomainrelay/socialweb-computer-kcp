package requester

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const guestSecretsEventPrefix = "secrets-"

func GuestSecretsEventName(contract string) string {
	name := guestSecretsEventPrefix + contract
	if len(name) > 253 {
		return name[:253]
	}
	return name
}

func guestSecretsKey(event v1alpha1.ContractEvent) bool {
	return event.Spec.Direction == v1alpha1.ContractEventInbound && event.Spec.NSID == market.ComputeEventsGuestPublicKeyNSID &&
		event.Spec.Tag == market.GuestSecretsTag && event.Status.Record != nil
}

func (q *Requester) latestGuestSecretsKey(contract v1alpha1.ComputeContract) (v1alpha1.ContractEvent, bool) {
	keys := q.contractEvents.Filter(func(e v1alpha1.ContractEvent) bool {
		return guestSecretsKey(e) && e.Spec.ContractRef.Name == contract.Name && e.DeletionTimestamp == nil &&
			(contract.UID == "" || len(e.OwnerReferences) == 0 || ownedBy(e, contract))
	})
	var latest v1alpha1.ContractEvent
	found := false
	for _, e := range keys {
		if !found || latest.CreationTimestamp.Before(&e.CreationTimestamp) ||
			(latest.CreationTimestamp.Equal(&e.CreationTimestamp) && e.Name > latest.Name) {
			latest, found = e, true
		}
	}
	return latest, found
}

func (q *Requester) sealGuestSecrets(ctx context.Context, contract v1alpha1.ComputeContract) error {
	if !q.cfg.SealedSecrets() || contract.Spec.SecretsRef == nil || q.deps.ContractEvents == nil || contract.DeletionTimestamp != nil {
		return nil
	}
	state := contract.Status.Contract
	if !secretsRequestPending(state) {
		return nil
	}
	q.sealMu.Lock()
	defer q.sealMu.Unlock()
	key, ok := q.latestGuestSecretsKey(contract)
	if !ok {
		q.log.Info("requester_guest_secrets_waiting_for_key", "contract", contract.Name)
		return nil
	}
	name := GuestSecretsEventName(contract.Name)
	reply := state.SecretsRequested.Payload
	if existing, known := q.contractEvents.Get(name); known && existing.Spec.RecipientRef != nil && existing.Spec.RecipientRef.Name == key.Name &&
		existing.Spec.InReplyTo != nil && *existing.Spec.InReplyTo == reply {
		return nil
	}
	recipient, err := age.ParseX25519Recipient(strings.TrimSpace(key.Spec.Key))
	if err != nil {
		return fmt.Errorf("requester: the guest's secrets key %s is not an age X25519 recipient: %w", key.Name, err)
	}
	entries, err := q.guestSecretsBundle(ctx, contract)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	ciphertext, err := sealTo(recipient, plaintext)
	clear(plaintext)
	if err != nil {
		return fmt.Errorf("requester: sealing the secrets bundle: %w", err)
	}
	if len(ciphertext) > market.MaxGuestSecretsCiphertext {
		return fmt.Errorf("requester: the sealed secrets bundle is %d bytes, more than %d", len(ciphertext), market.MaxGuestSecretsCiphertext)
	}
	event := v1alpha1.ContractEvent{
		ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{contractOwnerReference(contract)}},
		Spec: v1alpha1.ContractEventSpec{
			ContractRef:  commonv1.LocalObjectRef{Name: contract.Name},
			Direction:    v1alpha1.ContractEventOutbound,
			NSID:         market.ComputeEventsGuestSecretsNSID,
			Ciphertext:   ciphertext,
			RecipientRef: &commonv1.LocalObjectRef{Name: key.Name},
			InReplyTo:    &reply,
		},
	}
	if err := q.deps.ContractEvents.ApplyContractEvent(ctx, event); err != nil {
		return err
	}
	stored := event
	if existing, known := q.contractEvents.Get(name); known {
		stored = existing
		stored.Spec = event.Spec
	}
	q.contractEvents.Put(name, stored)
	q.log.Info("requester_guest_secrets_sealed", "contract", contract.Name, "contractEvent", name, "recipient", key.Name,
		"request", reply.URI, "files", len(entries), "bytes", len(ciphertext))
	return nil
}

func (q *Requester) guestSecretsBundle(ctx context.Context, contract v1alpha1.ComputeContract) ([]SecretEntry, error) {
	entries, err := q.readSecretsBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		return nil, err
	}
	if join, grant := contract.Spec.NodeJoin, contract.Status.Grant; join != nil && grant != nil {
		doc, err := json.Marshal(nodeClaimDocument(join.ClusterRef.Name, grant.Role, grant.Issuer))
		if err != nil {
			return nil, err
		}
		entries = append(entries, SecretEntry{Path: cloudinit.NodeClaimDocPath, Value: string(doc) + "\n"})
	}
	if len(entries) == 0 {
		return nil, errors.New("requester: the secrets bundle names no file")
	}
	return entries, nil
}

func sealTo(recipient *age.X25519Recipient, plaintext []byte) (string, error) {
	var out bytes.Buffer
	armored := armor.NewWriter(&out)
	w, err := age.Encrypt(armored, recipient)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(plaintext); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := armored.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

package bidder

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

const (
	provisionSyncPrefix = "accept-"

	releaseSyncPrefix = "release-"

	reportSyncPrefix = "report-"

	guestEventSyncPrefix = "guest-"

	guestEventSyncKeyPrefix = "guest:"
)

type marketEventRequest struct {
	URI string `json:"uri"`

	CID string `json:"cid"`

	Record map[string]any `json:"record"`
}

type provisionRef struct {
	URI string `json:"uri"`

	CID string `json:"cid"`
}

type provisionPayload struct {
	Accept provisionRef `json:"accept"`

	RequesterDID string `json:"requesterDid"`
}

type releasePayload struct {
	ReceiptURI string `json:"receiptUri"`

	RequesterDID string `json:"requesterDid"`

	Event marketEventRequest `json:"event"`
}

func normalizedPayload(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	return json.Marshal(stored)
}

func outboundSync(name string, labels map[string]string, owners []metav1.OwnerReference, source *v1alpha1.StateSyncSource,
	adapter, key string, payload json.RawMessage) v1alpha1.StateSync {
	return v1alpha1.StateSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: owners},
		Spec: v1alpha1.StateSyncSpec{
			Source:         source,
			Sink:           v1alpha1.StateSyncSink{Adapter: adapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        payload,
			IdempotencyKey: key,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
}

func (b *Bidder) createSync(ctx context.Context, sync v1alpha1.StateSync, kind string, fields ...any) error {
	err := b.intents.CreateStateSync(ctx, sync)
	if errors.Is(err, contract.ErrExists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bidder: creating the %s StateSync %s: %w", kind, sync.Name, err)
	}
	b.log.Info("a "+kind+" StateSync is created", append(fields, "stateSync", sync.Name)...)
	return nil
}

func vmSource(workspace string, vm *v1alpha1.ComputeVM) *v1alpha1.StateSyncSource {
	return &v1alpha1.StateSyncSource{Workspace: workspace, ApiVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM", Name: vm.Name, UID: string(vm.UID)}
}

func reportSyncName(uid, collection, tag string) string {
	key := collection
	if tag != "" {
		key += "#" + tag
	}
	sum := sha256.Sum256([]byte(key + "\x00" + uid))
	return reportSyncPrefix + hex.EncodeToString(sum[:8])
}

func reportSyncKey(collection, tag, receiptURI string) string {
	switch {
	case collection == market.ComputeEventsVMOnNetworkNSID && tag != "":
		return "onNetwork:" + tag + ":" + receiptURI
	case collection == market.ComputeEventsVMOnNetworkNSID:
		return "onNetwork:" + receiptURI
	case collection == market.ComputeEventsVMHostKeysNSID:
		return "hostKeys:" + receiptURI
	default:
		return collection + ":" + receiptURI
	}
}

func guestEventSyncName(uid, collection, tag, kind, recordSum string) string {
	sum := sha256.Sum256([]byte(collection + "\x00" + tag + "\x00" + kind + "\x00" + recordSum + "\x00" + uid))
	return guestEventSyncPrefix + hex.EncodeToString(sum[:8])
}

func guestEventSyncKey(collection, tag, kind, recordSum, receiptURI string) string {
	return guestEventSyncKeyPrefix + collection + ":" + tag + ":" + kind + ":" + recordSum + ":" + receiptURI
}

func (b *Bidder) ensureGuestEventSync(ctx context.Context, vm *v1alpha1.ComputeVM, collection, tag, kind string, record map[string]any) (string, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	recordSum := hex.EncodeToString(sum[:])
	name := guestEventSyncName(string(vm.UID), collection, tag, kind, recordSum)
	key := func(receiptURI string) string { return guestEventSyncKey(collection, tag, kind, recordSum, receiptURI) }
	return name, b.ensureEventSync(ctx, vm, name, key, collection, record, "guest event")
}

func (b *Bidder) ensureReportSync(ctx context.Context, vm *v1alpha1.ComputeVM, collection, tag string, record map[string]any) (string, error) {
	name := reportSyncName(string(vm.UID), collection, tag)
	key := func(receiptURI string) string { return reportSyncKey(collection, tag, receiptURI) }
	return name, b.ensureEventSync(ctx, vm, name, key, collection, record, "market report")
}

func (b *Bidder) ensureEventSync(ctx context.Context, vm *v1alpha1.ComputeVM, name string, key func(receiptURI string) string, collection string,
	record map[string]any, kind string) error {
	terms := vm.Spec.Contract
	if terms == nil {
		return fmt.Errorf("bidder: a %s StateSync needs a contract", kind)
	}
	if terms.Receipt.URI == "" || terms.Receipt.CID == "" {
		return fmt.Errorf("bidder: a %s StateSync needs a receipt", kind)
	}
	if vm.UID == "" {
		return fmt.Errorf("bidder: a %s StateSync needs a ComputeVM UID", kind)
	}
	envelope := map[string]any{
		"collection": collection,
		"receipt":    map[string]any{"uri": terms.Receipt.URI, "cid": terms.Receipt.CID},
		"record":     record,
	}
	if terms.SubmitEventURL != "" {
		envelope["submitEventUrl"] = terms.SubmitEventURL
	}
	payload, err := normalizedPayload(envelope)
	if err != nil {
		return err
	}
	owners := []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM", Name: vm.Name, UID: vm.UID}}
	sync := outboundSync(name, b.guests.SyncLabels(vm), owners, vmSource(b.cfg.Workspace, vm), b.guests.EventAdapter(),
		key(terms.Receipt.URI), payload)
	return b.createSync(ctx, sync, kind, "computevm", vm.Name, "collection", collection)
}

func (b *Bidder) ensureTerminationSync(ctx context.Context, info contractInfo, observedAt string, request *commonv1.StrongRef) (string, error) {
	held, ok := b.vms.Get(info.computeVM)
	if !ok {
		return "", fmt.Errorf("bidder: the ComputeVM %s does not exist", info.computeVM)
	}
	vm := held.DeepCopy()
	if terms := vm.Spec.Contract; terms == nil || terms.Receipt.URI == "" || terms.SubmitEventURL == "" {
		if vm.Spec.Contract == nil {
			vm.Spec.Contract = &v1alpha1.ComputeVMContract{}
		}
		if vm.Spec.Contract.Receipt.URI == "" {
			vm.Spec.Contract.Receipt.URI = info.receiptURI
			vm.Spec.Contract.Receipt.CID = info.receiptCID
		}
		vm.Spec.Contract.SubmitEventURL = cmp.Or(vm.Spec.Contract.SubmitEventURL, info.submitEventURL)
	}
	sync, err := b.guests.Termination(vm, b.cfg.Workspace, releaseReason, observedAt, request)
	if err != nil {
		return "", err
	}
	if err := b.createSync(ctx, sync, "termination", "computevm", vm.Name); err != nil {
		return "", err
	}
	return sync.Name, nil
}

func (b *Bidder) ensureInboundTerminationSync(ctx context.Context, vm *v1alpha1.ComputeVM, request *commonv1.StrongRef, requesterDID, eventURI, eventCID string, record map[string]any) error {
	sync, err := b.guests.Termination(vm, b.cfg.Workspace, releaseReason, b.now().UTC().Format(time.RFC3339), request)
	if err != nil {
		return err
	}
	envelope := map[string]any{}
	if err := json.Unmarshal(sync.Spec.Payload, &envelope); err != nil {
		return fmt.Errorf("bidder: the termination StateSync has no payload: %w", err)
	}
	envelope["requestEvent"] = map[string]any{"uri": eventURI, "cid": eventCID, "record": record}
	envelope["requesterDid"] = requesterDID
	if sync.Spec.Payload, err = normalizedPayload(envelope); err != nil {
		return err
	}
	return b.createSync(ctx, sync, "termination", "computevm", vm.Name)
}

func provisionSyncName(acceptURI string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(acceptURI)))
	return provisionSyncPrefix + hex.EncodeToString(sum[:8])
}

func (b *Bidder) ensureProvisionSync(ctx context.Context, request provisionPayload) (string, error) {
	if b.guests.AcceptKey(request.Accept.URI, request.Accept.CID) == "" {
		return "", errors.New("bidder: a provision StateSync needs an accept")
	}
	payload, err := normalizedPayload(request)
	if err != nil {
		return "", err
	}
	name := provisionSyncName(request.Accept.URI)
	sync := outboundSync(name, map[string]string{adapterLabel: ProvisionSyncAdapter}, nil, &v1alpha1.StateSyncSource{Workspace: b.cfg.Workspace},
		ProvisionSyncAdapter, "accept:"+request.Accept.URI, payload)
	if err := b.createSync(ctx, sync, "provision", "accept", request.Accept.URI); err != nil {
		return "", err
	}
	return name, nil
}

func releaseSyncName(receiptURI, eventURI string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(receiptURI) + "\x00" + strings.TrimSpace(eventURI)))
	return releaseSyncPrefix + hex.EncodeToString(sum[:8])
}

func (b *Bidder) ensureReleaseSync(ctx context.Context, receiptURI, requesterDID, eventURI, eventCID string, record map[string]any) error {
	request := releasePayload{ReceiptURI: receiptURI, RequesterDID: requesterDID,
		Event: marketEventRequest{URI: eventURI, CID: eventCID, Record: record}}
	payload, err := normalizedPayload(request)
	if err != nil {
		return err
	}
	sync := outboundSync(releaseSyncName(receiptURI, eventURI), map[string]string{adapterLabel: ReleaseSyncAdapter}, nil,
		&v1alpha1.StateSyncSource{Workspace: b.cfg.Workspace}, ReleaseSyncAdapter, "release:"+receiptURI+"#"+eventURI, payload)
	return b.createSync(ctx, sync, "release", "receipt", receiptURI)
}

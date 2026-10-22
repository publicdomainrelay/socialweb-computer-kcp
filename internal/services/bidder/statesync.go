package bidder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"
)

const (
	StateSyncInterval = 2 * time.Second

	TerminationSyncTimeout = 30 * time.Second

	ProvisionSyncAdapter = "compute.provision"

	provisionSyncPrefix = "accept-"

	stateSyncPollInterval = 500 * time.Millisecond
)

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
	if strings.TrimSpace(payload.Collection) == "" {
		return payload, nil, errors.New("bidder: the market event payload names no collection")
	}
	if payload.Receipt.URI == "" || payload.Receipt.CID == "" {
		return payload, nil, errors.New("bidder: the market event payload names no receipt")
	}
	if payload.Record == nil {
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
	prefix = strings.ToLower(prefix)
	var clean strings.Builder
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			clean.WriteRune(r)
		}
	}
	prefix = clean.String()
	if prefix == "" {
		return "market"
	}
	if prefix == "terminated" {
		return "termination"
	}
	return prefix
}

func unsignedRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if key == "signatures" {
			continue
		}
		out[key] = value
	}
	return out
}

func marketEventHash(envelope map[string]any, record map[string]any) (string, error) {
	unsigned := make(map[string]any, len(envelope))
	for key, value := range envelope {
		unsigned[key] = value
	}
	unsigned["record"] = record
	raw, err := json.Marshal(unsigned)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func recordObservedAt(record map[string]any) string {
	for _, key := range []string{"observedAt", "createdAt"} {
		if value, ok := record[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

type marketEventAdapter struct {
	records RecordStore

	did string

	attestation *market.Signer

	call ServiceCall

	log *slog.Logger
}

func (a *marketEventAdapter) Name() string { return computevm.TerminationSyncAdapter }

func (a *marketEventAdapter) logf() *slog.Logger {
	if a == nil || a.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.log
}

func (a *marketEventAdapter) Observe(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, envelope, err := decodeMarketEventPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	payloadRec, err := a.records.GetRecord(ctx, a.did, payload.Collection, payloadRKey)
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return statesync.Observation{Exists: false}, nil
		}
		return statesync.Observation{}, err
	}
	eventRec, err := a.records.GetRecord(ctx, a.did, market.EventNSID, eventRKey)
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return statesync.Observation{Exists: false}, nil
		}
		return statesync.Observation{}, err
	}
	hash, err := marketEventHash(envelope, unsignedRecord(payloadRec.Value))
	if err != nil {
		return statesync.Observation{}, err
	}
	return statesync.Observation{
		Exists: true,
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

func (a *marketEventAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	if a.attestation == nil || a.did == "" {
		return statesync.Observation{}, errors.New("bidder: the market event adapter has no attestation key or DID")
	}
	payload, _, err := decodeMarketEventPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	record := unsignedRecord(payload.Record)
	signed, err := market.SignRecord(record, a.did, a.attestation)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: signing the market event payload: %w", err)
	}
	written, err := a.records.PutRecord(ctx, a.did, payload.Collection, payloadRKey, signed)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: writing the market event payload: %w", err)
	}
	createdAt := recordObservedAt(record)
	event, err := market.PutEvent(ctx, a.records, a.did, eventRKey, market.EventOptions{
		Receipt:   market.RecordRef{URI: payload.Receipt.URI, CID: payload.Receipt.CID},
		Payload:   market.RecordRef{URI: written.URI, CID: written.CID},
		CreatedAt: createdAt,
	}, a.attestation)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: writing the market event: %w", err)
	}
	submitEventURL := strings.TrimSpace(payload.SubmitEventURL)
	if submitEventURL == "" {
		submitEventURL = strings.TrimSpace(request.Sink.SubmitEventUrl)
	}
	if submitEventURL != "" && a.call != nil {
		body := map[string]any{"uri": event.URI, "cid": event.CID, "record": event.Record}
		if err := a.call(ctx, submitEventURL, market.SubmitEventNSID, market.SubmitEventNSID, body); err != nil && ctx.Err() == nil {
			a.logf().Warn("submitting the market event failed; the records are on the PDS and the firehose carries them",
				"event", event.URI, "err", err.Error())
		}
	}
	return a.Observe(ctx, request)
}

func (a *marketEventAdapter) Delete(ctx context.Context, request statesync.Request) error {
	payload, _, err := decodeMarketEventPayload(request.Payload)
	if err != nil {
		return err
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	for _, item := range []struct {
		collection string
		rkey       string
	}{
		{payload.Collection, payloadRKey},
		{market.EventNSID, eventRKey},
	} {
		if err := a.records.DeleteRecord(ctx, a.did, item.collection, item.rkey); err != nil && !errors.Is(err, market.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) startStateSync() error {
	if s.cfg.Deps.KCP == nil {
		return errors.New("bidder: the component has no kcp clients")
	}
	registry := statesync.NewRegistry()
	adapter := &marketEventAdapter{records: s.records, did: s.did, attestation: s.attestation, call: s.callService, log: s.log}
	if err := registry.Register(adapter); err != nil {
		return err
	}
	provision := &provisionAdapter{service: s, log: s.log}
	if err := registry.Register(provision); err != nil {
		return err
	}
	kick := make(chan struct{}, 1)
	controller, err := statesync.NewController(statesync.Options{
		Workspace: s.cfg.Workspace,
		Dynamic:   s.syncDynamic,
		Registry:  registry,
		Log:       s.log,
		Interval:  StateSyncInterval,
		Kick:      kick,
	})
	if err != nil {
		return err
	}
	s.syncKick = kick
	s.syncController = controller
	return nil
}

func (s *Service) syncDynamic(workspace string) (dynamic.Interface, error) {
	if s.cfg.Deps.KCP == nil {
		return nil, errors.New("bidder: the component has no kcp clients")
	}
	client, err := s.cfg.Deps.KCP.Client(workspace)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(client.RESTConfig())
	if cfg == nil || strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("bidder: the kcp client has no rest config")
	}
	if !strings.Contains(cfg.Host, "/clusters/") {
		cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + workspace
	}
	return dynamic.NewForConfig(cfg)
}

func (s *Service) kickStateSync() {
	if s.syncKick == nil {
		return
	}
	select {
	case s.syncKick <- struct{}{}:
	default:
	}
}

func reportSyncName(uid, collection string) string {
	sum := sha256.Sum256([]byte(collection + "\x00" + uid))
	return "report-" + hex.EncodeToString(sum[:8])
}

func reportSyncKey(collection, receiptURI string) string {
	switch collection {
	case market.ComputeEventsVMOnNetworkNSID:
		return "onNetwork:" + receiptURI
	case market.ComputeEventsVMHostKeysNSID:
		return "hostKeys:" + receiptURI
	default:
		return collection + ":" + receiptURI
	}
}

func (s *Service) ensureReportSync(ctx context.Context, vm *v1alpha1.ComputeVM, collection string, record map[string]any) (string, error) {
	if vm == nil || vm.Spec.Contract == nil {
		return "", errors.New("bidder: a report StateSync needs a contract")
	}
	contract := vm.Spec.Contract
	if contract.ReceiptUri == "" || contract.ReceiptCid == "" {
		return "", errors.New("bidder: a report StateSync needs a receipt")
	}
	if vm.UID == "" {
		return "", errors.New("bidder: a report StateSync needs a ComputeVM UID")
	}
	payload := map[string]any{
		"collection": collection,
		"receipt":    map[string]any{"uri": contract.ReceiptUri, "cid": contract.ReceiptCid},
		"record":     record,
	}
	if contract.SubmitEventUrl != "" {
		payload["submitEventUrl"] = contract.SubmitEventUrl
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", err
	}
	name := reportSyncName(string(vm.UID), collection)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "StateSync",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				computevm.TerminationSyncSourceLabel: computevm.TerminationSyncSource,
				computevm.TerminationSyncUIDLabel:    string(vm.UID),
				computevm.TerminationSyncVMNameLabel: vm.Name,
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": v1alpha1.GroupVersion.String(),
				"kind":       "ComputeVM",
				"name":       vm.Name,
				"uid":        string(vm.UID),
			}},
		},
		"spec": map[string]any{
			"source": map[string]any{
				"workspace":  s.cfg.Workspace,
				"apiVersion": v1alpha1.GroupVersion.String(),
				"kind":       "ComputeVM",
				"name":       vm.Name,
				"uid":        string(vm.UID),
			},
			"sink":           map[string]any{"adapter": computevm.TerminationSyncAdapter},
			"direction":      string(v1alpha1.StateSyncDirectionOutbound),
			"payload":        stored,
			"idempotencyKey": reportSyncKey(collection, contract.ReceiptUri),
			"conflictPolicy": string(v1alpha1.StateSyncConflictSourceWins),
			"deletionPolicy": string(v1alpha1.StateSyncDeletionRetain),
		},
	}}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return "", err
	}
	if _, err := dyn.Resource(statesync.StateSyncGVR).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("bidder: creating the report StateSync %s: %w", name, err)
		}
	} else {
		s.log.Info("a market report StateSync is created", "computevm", vm.Name, "collection", collection, "statesync", name)
	}
	s.kickStateSync()
	return name, nil
}

func (s *Service) observedReportURI(ctx context.Context, name string) string {
	if name == "" {
		return ""
	}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return ""
	}
	u, err := dyn.Resource(statesync.StateSyncGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if v1alpha1.StateSyncPhase(phase) != v1alpha1.StateSyncPhaseAcked {
		return ""
	}
	ref, _, _ := unstructured.NestedString(u.Object, "status", "acked", "ref")
	return strings.TrimSpace(ref)
}

func (s *Service) ensureTerminationSync(ctx context.Context, info contractInfo, reason, observedAt string, request *v1alpha1.RecordRef) (string, error) {
	if s.vms == nil {
		return "", errors.New("bidder: the ComputeVM store is not configured")
	}
	vm, err := s.vms.Get(ctx, info.computeVM)
	if err != nil {
		return "", err
	}
	if vm == nil {
		return "", fmt.Errorf("bidder: the ComputeVM %s does not exist", info.computeVM)
	}
	if contract := vm.Spec.Contract; contract == nil || contract.ReceiptUri == "" || contract.SubmitEventUrl == "" {
		submitEventURL := info.submitEventURL
		if submitEventURL == "" {
			submitEventURL = s.publicURL()
		}
		vm = vm.DeepCopy()
		if vm.Spec.Contract == nil {
			vm.Spec.Contract = &v1alpha1.ComputeVMContract{}
		}
		if vm.Spec.Contract.ReceiptUri == "" {
			vm.Spec.Contract.ReceiptUri = info.receiptURI
			vm.Spec.Contract.ReceiptCid = info.receiptCID
		}
		if vm.Spec.Contract.SubmitEventUrl == "" {
			vm.Spec.Contract.SubmitEventUrl = submitEventURL
		}
	}
	object, err := computevm.TerminationStateSync(vm, s.cfg.Workspace, reason, observedAt, request)
	if err != nil {
		return "", err
	}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return "", err
	}
	name := object.GetName()
	if _, err := dyn.Resource(statesync.StateSyncGVR).Create(ctx, object, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("bidder: creating the termination StateSync %s: %w", name, err)
	}
	s.kickStateSync()
	return name, nil
}

func (s *Service) waitTerminationSync(ctx context.Context, name string, timeout time.Duration) error {
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(statesync.StateSyncGVR)
	deadline := time.Now().Add(timeout)
	for {
		u, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
			switch v1alpha1.StateSyncPhase(phase) {
			case v1alpha1.StateSyncPhaseAcked:
				return nil
			case v1alpha1.StateSyncPhaseStalled:
				lastError, _, _ := unstructured.NestedString(u.Object, "status", "lastError")
				return fmt.Errorf("the termination StateSync %s is Stalled: %s", name, lastError)
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the termination StateSync %s was not Acked within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stateSyncPollInterval):
		}
	}
}

func (s *Service) acknowledgedTermination(ctx context.Context, name string) *v1alpha1.ComputeVMTermination {
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return nil
	}
	u, err := dyn.Resource(statesync.StateSyncGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	payloadURI, _, _ := unstructured.NestedString(u.Object, "status", "acked", "ref")
	payloadCID, _, _ := unstructured.NestedString(u.Object, "status", "acked", "cid")
	eventURI, _, _ := unstructured.NestedString(u.Object, "status", "acked", "eventUri")
	eventCID, _, _ := unstructured.NestedString(u.Object, "status", "acked", "eventCid")
	at, _, _ := unstructured.NestedString(u.Object, "status", "acked", "at")
	if payloadURI == "" {
		return nil
	}
	return &v1alpha1.ComputeVMTermination{
		Reason:     "deleted_on_request",
		ObservedAt: at,
		PayloadUri: payloadURI,
		PayloadCid: payloadCID,
		EventUri:   eventURI,
		EventCid:   eventCID,
	}
}

type provisionRef struct {
	URI string `json:"uri"`

	CID string `json:"cid"`
}

type provisionPayload struct {
	Accept provisionRef `json:"accept"`

	RequesterDID string `json:"requesterDid"`
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

func provisionSyncName(acceptURI string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(acceptURI)))
	return provisionSyncPrefix + hex.EncodeToString(sum[:8])
}

func provisionPayloadHash(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (payload provisionPayload) acceptKey() string {
	return computevm.AcceptRef{URI: payload.Accept.URI, CID: payload.Accept.CID}.Key()
}

func provisionBidHintKey(payload provisionPayload) string {
	return "bid:" + payload.acceptKey()
}

func (s *Service) nowUTC() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) ensureProvisionSync(ctx context.Context, payload provisionPayload) (string, error) {
	if payload.acceptKey() == "" {
		return "", errors.New("bidder: a provision StateSync needs an accept")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", err
	}
	name := provisionSyncName(payload.Accept.URI)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "StateSync",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"statesync.socialweb.computer/adapter": ProvisionSyncAdapter,
			},
		},
		"spec": map[string]any{
			"source":         map[string]any{"workspace": s.cfg.Workspace},
			"sink":           map[string]any{"adapter": ProvisionSyncAdapter},
			"direction":      string(v1alpha1.StateSyncDirectionOutbound),
			"payload":        stored,
			"idempotencyKey": "accept:" + payload.Accept.URI,
			"conflictPolicy": string(v1alpha1.StateSyncConflictSourceWins),
			"deletionPolicy": string(v1alpha1.StateSyncDeletionRetain),
		},
	}}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return "", err
	}
	if _, err := dyn.Resource(statesync.StateSyncGVR).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("bidder: creating the provision StateSync %s: %w", name, err)
		}
	} else {
		s.log.Info("a provision StateSync is created", "accept", payload.Accept.URI, "statesync", name)
	}
	return name, nil
}

type provisionAdapter struct {
	service *Service

	log *slog.Logger
}

func (a *provisionAdapter) Name() string { return ProvisionSyncAdapter }

func (a *provisionAdapter) logf() *slog.Logger {
	if a == nil || a.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.log
}

func (s *Service) bidForAcceptIntent(ctx context.Context, payload provisionPayload) (*v1alpha1.Bid, error) {
	if s.bids == nil {
		return nil, errors.New("bidder: the bidder has no Bid store")
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range bids {
		accept := bids[i].Status.Accept
		if accept == nil || accept.Uri != payload.Accept.URI || accept.Cid != payload.Accept.CID {
			continue
		}
		return &bids[i], nil
	}
	return nil, nil
}

func (a *provisionAdapter) findVM(ctx context.Context, payload provisionPayload, bid *v1alpha1.Bid) (*v1alpha1.ComputeVM, error) {
	service := a.service
	if service == nil || service.vms == nil {
		return nil, errors.New("bidder: the provision adapter has no ComputeVM store")
	}
	if bid != nil && bid.Status.ComputeVM != "" {
		vm, err := service.vms.Get(ctx, bid.Status.ComputeVM)
		if err != nil {
			return nil, err
		}
		if vm != nil {
			return vm, nil
		}
	}
	key := payload.acceptKey()
	list, err := service.vms.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if computevm.AcceptKeyOf(&list[i]) == key {
			return &list[i], nil
		}
	}
	return nil, nil
}

func contractMatchesAccept(vm *v1alpha1.ComputeVM, payload provisionPayload) bool {
	if vm == nil || vm.Spec.Contract == nil {
		return false
	}
	contract := vm.Spec.Contract
	return contract.AcceptUri == payload.Accept.URI && contract.AcceptCid == payload.Accept.CID && contract.ReceiptUri != ""
}

func provisionObservation(vm *v1alpha1.ComputeVM, payload provisionPayload, request statesync.Request, at time.Time) statesync.Observation {
	observation := statesync.Observation{
		Exists: true,
		Ref:    vm.Name,
		CID:    string(vm.UID),
		Hash:   provisionPayloadHash(request.Payload),
		At:     at.Format(time.RFC3339),
		Meta: map[string]string{
			"computevm": vm.Name,
			"acceptUri": payload.Accept.URI,
			"acceptCid": payload.Accept.CID,
		},
	}
	if contract := vm.Spec.Contract; contract != nil {
		observation.Meta["receiptUri"] = contract.ReceiptUri
		observation.Meta["receiptCid"] = contract.ReceiptCid
	}
	return observation
}

func provisionSettledObservation(ref string, payload provisionPayload, request statesync.Request) statesync.Observation {
	return statesync.Observation{
		Exists: true,
		Ref:    ref,
		Hash:   provisionPayloadHash(request.Payload),
		At:     time.Now().UTC().Format(time.RFC3339),
		Meta: map[string]string{
			"acceptUri": payload.Accept.URI,
			"acceptCid": payload.Accept.CID,
		},
	}
}

func (a *provisionAdapter) bidForIntent(ctx context.Context, payload provisionPayload) (*v1alpha1.Bid, error) {
	service := a.service
	if value, ok := service.contracts.Load(provisionBidHintKey(payload)); ok {
		if info, isInfo := value.(contractInfo); isInfo && info.bidName != "" {
			bid, err := service.bids.Get(ctx, info.bidName)
			if err != nil {
				return nil, err
			}
			if bid != nil {
				return bid, nil
			}
		}
	}
	return service.bidForAcceptIntent(ctx, payload)
}

func (a *provisionAdapter) Observe(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, err := decodeProvisionPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	service := a.service
	if service == nil || service.vms == nil {
		return statesync.Observation{}, errors.New("bidder: the provision adapter has no ComputeVM store")
	}
	var bid *v1alpha1.Bid
	if service.bids != nil {
		bid, err = a.bidForIntent(ctx, payload)
		if err != nil {
			return statesync.Observation{}, err
		}
	}
	if bid != nil && bid.Status.Terminated != nil {
		return provisionSettledObservation("terminated:"+bid.Status.Terminated.ReceiptUri, payload, request), nil
	}
	vm, err := a.findVM(ctx, payload, bid)
	if err != nil {
		return statesync.Observation{}, err
	}
	if vm == nil {
		if bid != nil && bid.Status.ComputeVM != "" {
			return provisionSettledObservation("released:"+bid.Status.ComputeVM, payload, request), nil
		}
		return statesync.Observation{Exists: false}, nil
	}
	if !contractMatchesAccept(vm, payload) {
		return statesync.Observation{Exists: false}, nil
	}
	return provisionObservation(vm, payload, request, time.Now().UTC()), nil
}

func (a *provisionAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	service := a.service
	if service == nil || service.provider == nil {
		return statesync.Observation{}, errors.New("bidder: the provision adapter has no compute provider")
	}
	if service.records == nil || service.vms == nil || service.attestation == nil || service.did == "" {
		return statesync.Observation{}, errors.New("bidder: the provision adapter is not ready")
	}
	payload, err := decodeProvisionPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	unlock := service.lockAccept(payload.Accept.URI)
	defer unlock()

	acceptResolved, err := service.records.Resolve(ctx, market.RecordRef{URI: payload.Accept.URI, CID: payload.Accept.CID})
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: resolving the accept: %w", err)
	}
	accept, err := market.ParseAccept(acceptResolved.Value)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: the accept is invalid: %w", err)
	}
	if err := service.verifyRecord(ctx, acceptResolved); err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: the accept is not signed by its author: %w", err)
	}
	if scope := service.policy.Scope(ctx, PolicyInput{SelfDid: service.did, CounterpartyDid: payload.RequesterDID}); !scope.Allow {
		a.logf().Info("the scope policy declined the accept", "accept", payload.Accept.URI, "reason", scope.Reason)
		return statesync.Observation{}, errors.New("bidder: the scope policy declined the accept: " + scope.Reason)
	}
	bid, err := service.bidForAccept(ctx, accept)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: finding the bid of the accept: %w", err)
	}
	if bid.Spec.Requester != "" && bid.Spec.Requester != payload.RequesterDID {
		return statesync.Observation{}, errors.New("bidder: the accept is not from the requester that opened the contract")
	}
	if terminated := bid.Status.Terminated; terminated != nil {
		if bid.Status.Accept == nil || bid.Status.Accept.Uri != payload.Accept.URI || bid.Status.Accept.Cid != payload.Accept.CID {
			return statesync.Observation{}, errors.New("bidder: the bid's contract was released")
		}
		return provisionSettledObservation("terminated:"+terminated.ReceiptUri, payload, request), nil
	}
	if bid.Status.Phase == v1alpha1.BidPhaseFailed {
		return statesync.Observation{}, errors.New("bidder: the bid failed: " + bid.Status.Message)
	}
	now := service.nowUTC()
	if bidExpired(bid, now) || bid.Status.Phase == v1alpha1.BidPhaseExpired {
		if bid.Status.Phase == v1alpha1.BidPhaseDeciding || bid.Status.Phase == v1alpha1.BidPhasePublished {
			service.expireBid(ctx, bid, now)
		}
		return statesync.Observation{}, errors.New("bidder: the bid has expired")
	}

	rfpResolved, err := service.records.Resolve(ctx, market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID})
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: resolving the accepted RFP: %w", err)
	}
	rfp, err := market.ParseRFP(rfpResolved.Value)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: the accepted RFP is invalid: %w", err)
	}
	if err := service.verifyRecord(ctx, rfpResolved); err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: the accepted RFP is not signed by its author: %w", err)
	}
	decision := service.policy.EvaluatePolicies(ctx, rfp.Policies, PolicyInput{
		Perspective:      PolicyPerspectiveBidder,
		SelfDid:          service.did,
		SubjectDid:       service.did,
		CounterpartyDid:  payload.RequesterDID,
		RootRequesterDid: payload.RequesterDID,
		RFP:              &PolicyStrongRef{URI: accept.RFP.URI, CID: accept.RFP.CID},
		Mode:             PolicyModeFull,
	})
	if !decision.Allow {
		a.logf().Warn("policy rejected the accept", "accept", payload.Accept.URI, "reason", decision.Reason, "run", decision.Run)
		return statesync.Observation{}, errors.New("bidder: the policy rejected the accept: " + decision.Reason)
	}
	payloadResolved, err := service.records.Resolve(ctx, market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: resolving the RFP payload: %w", err)
	}
	bidResolved, err := service.records.Resolve(ctx, market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID})
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: resolving the accepted bid: %w", err)
	}
	parsedBid, err := market.ParseBid(bidResolved.Value)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: the accepted bid is invalid: %w", err)
	}
	if parsedBid.Config == nil {
		return statesync.Observation{}, errors.New("bidder: the accepted bid names no bid config")
	}
	configResolved, err := service.records.Resolve(ctx, market.RecordRef{URI: parsedBid.Config.URI, CID: parsedBid.Config.CID})
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: resolving the bid config: %w", err)
	}
	submitEventURL := service.submitEventURL(ctx, accept.SubmitEvent)
	if strings.TrimSpace(accept.SubmitEvent) != "" && submitEventURL == "" {
		a.logf().Warn("the accept's submitEvent does not resolve; the firehose carries the event", "accept", payload.Accept.URI, "submitEvent", accept.SubmitEvent)
	}
	receiptURI, receiptCID, err := service.createReceipt(ctx, market.ReceiptRefs{
		RFP:         market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID},
		Bid:         market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID},
		Accept:      market.RecordRef{URI: payload.Accept.URI, CID: payload.Accept.CID},
		Payload:     &market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID},
		SubmitEvent: accept.SubmitEvent,
	}, acceptResolved, payload.RequesterDID)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: writing the receipt: %w", err)
	}
	status := cloneBidStatus(bid.Status)
	if status.Accept == nil || status.Accept.Uri != payload.Accept.URI || status.Accept.Cid != payload.Accept.CID {
		status.Accept = &v1alpha1.BidRecordRef{Uri: payload.Accept.URI, Cid: payload.Accept.CID}
		if status.Timings.AcceptedAt == "" {
			status.Timings.AcceptedAt = now.Format(time.RFC3339)
		}
		status.Phase = v1alpha1.BidPhaseWon
		setBidCondition(&status, bid.Generation, v1alpha1.BidConditionComplete, metav1.ConditionTrue, "ContractAccepted", "the guest contract is accepted")
		if err := service.bids.PatchStatus(ctx, bid.Name, status); err != nil {
			return statesync.Observation{}, fmt.Errorf("bidder: recording the accepted contract: %w", err)
		}
		bid.Status = status
	}
	result, err := service.provider.Provision(ctx, ProvisionRequest{
		RFPURI:         accept.RFP.URI,
		RFPCID:         accept.RFP.CID,
		VMURI:          rfp.Payload.URI,
		VMCID:          rfp.Payload.CID,
		AcceptURI:      payload.Accept.URI,
		AcceptCID:      payload.Accept.CID,
		BidURI:         accept.Bid.URI,
		BidCID:         accept.Bid.CID,
		RequesterDID:   payload.RequesterDID,
		SubmitEventURL: submitEventURL,
		VMPayload:      payloadResolved,
		BidConfig:      market.StrongRef{URI: parsedBid.Config.URI, CID: parsedBid.Config.CID},
		BidConfigValue: configResolved.Value,
	})
	if err != nil {
		return statesync.Observation{}, err
	}
	contract := v1alpha1.ComputeVMContract{
		ReceiptUri:     receiptURI,
		ReceiptCid:     receiptCID,
		AcceptUri:      payload.Accept.URI,
		AcceptCid:      payload.Accept.CID,
		SubmitEventUrl: submitEventURL,
		AcceptedAt:     now.Format(time.RFC3339),
	}
	if err := service.provider.BindContract(ctx, result.Name, contract); err != nil {
		return statesync.Observation{}, err
	}
	info := contractInfo{
		bidName:        bid.Name,
		computeVM:      result.Name,
		receiptURI:     receiptURI,
		receiptCID:     receiptCID,
		acceptURI:      payload.Accept.URI,
		acceptCID:      payload.Accept.CID,
		acceptAuthor:   payload.RequesterDID,
		submitEventURL: submitEventURL,
	}
	service.contracts.Store(receiptURI, info)
	service.contracts.Store(provisionBidHintKey(payload), info)
	if service.bids != nil {
		current, err := service.bids.Get(ctx, bid.Name)
		if err != nil {
			return statesync.Observation{}, err
		}
		if current != nil {
			if current.Status.ComputeVM != result.Name {
				status := cloneBidStatus(current.Status)
				status.ComputeVM = result.Name
				status.Phase = v1alpha1.BidPhaseWon
				if status.Timings.AcceptedAt == "" {
					status.Timings.AcceptedAt = now.Format(time.RFC3339)
				}
				if err := service.bids.PatchStatus(ctx, current.Name, status); err != nil {
					return statesync.Observation{}, err
				}
			}
			if terminated := current.Status.Terminated; terminated != nil {
				annotations := map[string]string{
					computevm.ReleaseRequestedAnnotation: terminated.At,
					computevm.ReleaseRequestAnnotation:   terminated.PayloadUri + "#" + terminated.PayloadCid,
				}
				if err := service.vms.PatchAnnotations(ctx, result.Name, annotations); err != nil {
					return statesync.Observation{}, err
				}
				var requestRef *v1alpha1.RecordRef
				if terminated.PayloadUri != "" {
					requestRef = &v1alpha1.RecordRef{URI: terminated.PayloadUri, CID: terminated.PayloadCid}
				}
				observedAt := terminated.At
				if observedAt == "" {
					observedAt = now.Format(time.RFC3339)
				}
				if _, err := service.ensureTerminationSync(ctx, info, "deleted_on_request", observedAt, requestRef); err != nil {
					a.logf().Warn("resuming the termination of a released accept failed", "bid", bid.Name, "err", err.Error())
				}
			}
		}
	}
	return a.Observe(ctx, request)
}

func (a *provisionAdapter) Delete(ctx context.Context, request statesync.Request) error {
	return nil
}

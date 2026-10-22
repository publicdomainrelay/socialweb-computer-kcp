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

type provisionRecord struct {
	URI string `json:"uri"`

	CID string `json:"cid"`

	Value map[string]any `json:"value,omitempty"`
}

type provisionPayload struct {
	BidName string `json:"bidName"`

	Accept provisionRef `json:"accept"`

	Bid provisionRef `json:"bid"`

	RFP provisionRef `json:"rfp"`

	Payload provisionRecord `json:"payload"`

	RequesterDID string `json:"requesterDid"`

	Receipt provisionRef `json:"receipt"`

	SubmitEvent string `json:"submitEvent,omitempty"`

	BidConfig provisionRecord `json:"bidConfig"`
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
	case strings.TrimSpace(payload.BidName) == "":
		return payload, errors.New("bidder: the provision payload names no bid")
	case payload.Accept.URI == "" || payload.Accept.CID == "":
		return payload, errors.New("bidder: the provision payload names no accept")
	case payload.Bid.URI == "" || payload.Bid.CID == "":
		return payload, errors.New("bidder: the provision payload names no accepted bid")
	case payload.RFP.URI == "" || payload.RFP.CID == "":
		return payload, errors.New("bidder: the provision payload names no RFP")
	case payload.Payload.URI == "" || payload.Payload.CID == "":
		return payload, errors.New("bidder: the provision payload names no VM payload")
	case payload.Receipt.URI == "" || payload.Receipt.CID == "":
		return payload, errors.New("bidder: the provision payload names no receipt")
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

func (s *Service) ensureProvisionSync(ctx context.Context, bid *v1alpha1.Bid, payload provisionPayload) (string, error) {
	if bid == nil {
		return "", errors.New("bidder: a provision StateSync needs a Bid")
	}
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
	metadata := map[string]any{
		"name": name,
		"labels": map[string]any{
			"statesync.socialweb.computer/adapter": ProvisionSyncAdapter,
		},
	}
	if bid.UID != "" {
		metadata["ownerReferences"] = []any{map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "Bid",
			"name":       bid.Name,
			"uid":        string(bid.UID),
		}}
	}
	source := map[string]any{
		"workspace":  s.cfg.Workspace,
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "Bid",
		"name":       bid.Name,
		"uid":        string(bid.UID),
	}
	if bid.Generation > 0 {
		source["generation"] = bid.Generation
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "StateSync",
		"metadata":   metadata,
		"spec": map[string]any{
			"source":         source,
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
		s.log.Info("a provision StateSync is created", "bid", bid.Name, "accept", payload.Accept.URI, "statesync", name)
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

func (a *provisionAdapter) findVM(ctx context.Context, payload provisionPayload) (*v1alpha1.ComputeVM, error) {
	service := a.service
	if service == nil || service.vms == nil {
		return nil, errors.New("bidder: the provision adapter has no ComputeVM store")
	}
	if service.bids != nil {
		bid, err := service.bids.Get(ctx, payload.BidName)
		if err != nil {
			return nil, err
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

func contractBound(vm *v1alpha1.ComputeVM, receipt provisionRef) bool {
	if vm == nil || vm.Spec.Contract == nil {
		return false
	}
	contract := vm.Spec.Contract
	return contract.ReceiptUri == receipt.URI && contract.ReceiptCid == receipt.CID
}

func (a *provisionAdapter) Observe(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, err := decodeProvisionPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	vm, err := a.findVM(ctx, payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	if vm == nil || !contractBound(vm, payload.Receipt) {
		return statesync.Observation{Exists: false}, nil
	}
	return statesync.Observation{
		Exists: true,
		Ref:    vm.Name,
		CID:    string(vm.UID),
		Hash:   provisionPayloadHash(request.Payload),
		At:     time.Now().UTC().Format(time.RFC3339),
		Meta: map[string]string{
			"computevm":  vm.Name,
			"receiptUri": payload.Receipt.URI,
			"receiptCid": payload.Receipt.CID,
		},
	}, nil
}

func (a *provisionAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	service := a.service
	if service == nil || service.provider == nil {
		return statesync.Observation{}, errors.New("bidder: the provision adapter has no compute provider")
	}
	payload, err := decodeProvisionPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	submitEventURL := service.submitEventURL(ctx, payload.SubmitEvent)
	result, err := service.provider.Provision(ctx, ProvisionRequest{
		RFPURI:         payload.RFP.URI,
		RFPCID:         payload.RFP.CID,
		VMURI:          payload.Payload.URI,
		VMCID:          payload.Payload.CID,
		AcceptURI:      payload.Accept.URI,
		AcceptCID:      payload.Accept.CID,
		BidURI:         payload.Bid.URI,
		BidCID:         payload.Bid.CID,
		RequesterDID:   payload.RequesterDID,
		SubmitEventURL: submitEventURL,
		VMPayload:      market.ResolvedRecord{URI: payload.Payload.URI, CID: payload.Payload.CID, Value: payload.Payload.Value},
		BidConfig:      market.StrongRef{URI: payload.BidConfig.URI, CID: payload.BidConfig.CID},
		BidConfigValue: payload.BidConfig.Value,
	})
	if err != nil {
		return statesync.Observation{}, err
	}
	now := time.Now().UTC()
	if service.cfg.Now != nil {
		now = service.cfg.Now().UTC()
	}
	contract := v1alpha1.ComputeVMContract{
		ReceiptUri:     payload.Receipt.URI,
		ReceiptCid:     payload.Receipt.CID,
		AcceptUri:      payload.Accept.URI,
		AcceptCid:      payload.Accept.CID,
		SubmitEventUrl: submitEventURL,
		AcceptedAt:     now.Format(time.RFC3339),
	}
	if err := service.provider.BindContract(ctx, result.Name, contract); err != nil {
		return statesync.Observation{}, err
	}
	info := contractInfo{
		bidName:        payload.BidName,
		computeVM:      result.Name,
		receiptURI:     payload.Receipt.URI,
		receiptCID:     payload.Receipt.CID,
		acceptURI:      payload.Accept.URI,
		acceptCID:      payload.Accept.CID,
		acceptAuthor:   payload.RequesterDID,
		submitEventURL: submitEventURL,
	}
	service.contracts.Store(payload.Receipt.URI, info)
	if service.bids != nil {
		bid, err := service.bids.Get(ctx, payload.BidName)
		if err != nil {
			return statesync.Observation{}, err
		}
		if bid != nil {
			if bid.Status.ComputeVM != result.Name {
				status := cloneBidStatus(bid.Status)
				status.ComputeVM = result.Name
				status.Phase = v1alpha1.BidPhaseWon
				if status.Timings.AcceptedAt == "" {
					status.Timings.AcceptedAt = now.Format(time.RFC3339)
				}
				if err := service.bids.PatchStatus(ctx, bid.Name, status); err != nil {
					return statesync.Observation{}, err
				}
			}
			if terminated := bid.Status.Terminated; terminated != nil {
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
					a.logf().Warn("resuming the termination of a released accept failed", "bid", payload.BidName, "err", err.Error())
				}
			}
		}
	}
	return a.Observe(ctx, request)
}

func (a *provisionAdapter) Delete(ctx context.Context, request statesync.Request) error {
	return nil
}

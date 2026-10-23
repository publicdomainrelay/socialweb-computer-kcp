package bidder

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	StateSyncInterval = 2 * time.Second

	TerminationSyncTimeout = 30 * time.Second

	ProvisionSyncAdapter = "compute.provision"

	ReleaseSyncAdapter = "compute.release"

	provisionSyncPrefix = "accept-"

	releaseSyncPrefix = "release-"

	stateSyncPollInterval = 500 * time.Millisecond
)

type marketEventRequest struct {
	URI string `json:"uri"`

	CID string `json:"cid"`

	Record map[string]any `json:"record"`
}

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

	RequestEvent *marketEventRequest `json:"requestEvent,omitempty"`

	RequesterDID string `json:"requesterDid,omitempty"`
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
	maps.Copy(unsigned, envelope)
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

func verifyInboundDeleteEvent(ctx context.Context, service *Service, event marketEventRequest, receiptURI, receiptCID, requesterDID string) (*market.Event, error) {
	if service == nil || service.records == nil {
		return nil, errors.New("bidder: the inbound event verifier is not configured")
	}
	uri := strings.TrimSpace(event.URI)
	if uri == "" || event.Record == nil {
		return nil, errors.New("bidder: the inbound event is empty")
	}
	parsed, err := market.ParseEvent(event.Record)
	if err != nil {
		return nil, fmt.Errorf("bidder: the inbound event is invalid: %w", err)
	}
	if market.NSIDFromURI(parsed.Payload.URI) != market.ComputeEventsVMDeleteNSID {
		return nil, errors.New("bidder: the inbound event is not a vm delete")
	}
	if err := service.verifyRecord(ctx, market.ResolvedRecord{URI: uri, CID: event.CID, Value: event.Record}); err != nil {
		return nil, fmt.Errorf("bidder: the inbound event is not signed: %w", err)
	}
	if market.ATURIAuthority(uri) != requesterDID {
		return nil, errors.New("bidder: the inbound event is not from the requester")
	}
	if parsed.Receipt.URI != receiptURI || parsed.Receipt.CID != receiptCID {
		return nil, errors.New("bidder: the inbound event names another receipt")
	}
	payloadResolved, err := service.records.Resolve(ctx, market.RecordRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID})
	if err != nil {
		return nil, fmt.Errorf("bidder: resolving the inbound event payload: %w", err)
	}
	if kind, _ := payloadResolved.Value["$type"].(string); kind != market.ComputeEventsVMDeleteNSID {
		return nil, errors.New("bidder: the inbound event payload is not a vm delete")
	}
	if err := service.verifyRecord(ctx, payloadResolved); err != nil {
		return nil, fmt.Errorf("bidder: the inbound event payload is not signed: %w", err)
	}
	return parsed, nil
}

type marketEventAdapter struct {
	service *Service

	records *market.RepoStore

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
	if payload.RequestEvent != nil {
		if err := a.applyInboundEvent(ctx, request, payload); err != nil {
			return statesync.Observation{}, err
		}
	}
	payloadRKey, eventRKey := payload.recordKeys(request.Key)
	record := unsignedRecord(payload.Record)
	signed, err := market.SignRecord(record, a.did, a.attestation)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: signing the market event payload: %w", err)
	}
	writer := createOnlyWriter{records: a.records, did: a.did}
	written, err := writer.PutRecord(ctx, a.did, payload.Collection, payloadRKey, signed)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: writing the market event payload: %w", err)
	}
	createdAt := recordObservedAt(record)
	event, err := market.PutEvent(ctx, writer, a.did, eventRKey, market.EventOptions{
		Receipt:   market.RecordRef{URI: payload.Receipt.URI, CID: payload.Receipt.CID},
		Payload:   market.RecordRef{URI: written.URI, CID: written.CID},
		CreatedAt: createdAt,
	}, a.attestation)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: writing the market event: %w", err)
	}
	submitEventURL := strings.TrimSpace(payload.SubmitEventURL)
	if submitEventURL == "" {
		submitEventURL = strings.TrimSpace(request.Sink.SubmitEventURL)
	}
	if submitEventURL != "" && a.call != nil && a.service.cfg.outboundXRPC() {
		body := map[string]any{"uri": event.URI, "cid": event.CID, "record": event.Record}
		if err := a.call(ctx, submitEventURL, market.SubmitEventNSID, market.SubmitEventNSID, body); err != nil {
			return statesync.Observation{}, classifyDeliveryError(err)
		}
	}
	return a.Observe(ctx, request)
}

func classifyDeliveryError(err error) error {
	var callErr *serviceCallError
	if errors.As(err, &callErr) && callErr.Status >= 400 && callErr.Status <= 499 && callErr.Status != 408 && callErr.Status != 429 {
		return fmt.Errorf("bidder: the requester rejected the market event (HTTP %d): %w", callErr.Status, statesync.ErrRejected)
	}
	return err
}

func (a *marketEventAdapter) applyInboundEvent(ctx context.Context, request statesync.Request, payload marketEventPayload) error {
	service := a.service
	if service == nil || service.vms == nil || service.bids == nil {
		return errors.New("bidder: the inbound event adapter is not configured")
	}
	event, err := verifyInboundDeleteEvent(ctx, service, *payload.RequestEvent, payload.Receipt.URI, payload.Receipt.CID, payload.RequesterDID)
	if err != nil {
		return err
	}
	if request.Source == nil || strings.TrimSpace(request.Source.Name) == "" {
		return errors.New("bidder: the inbound event names no ComputeVM")
	}
	vm, err := service.vms.Get(ctx, request.Source.Name)
	if err != nil {
		return err
	}
	if vm == nil {
		a.logf().Warn("the ComputeVM of an inbound delete event is gone", "computevm", request.Source.Name)
		return nil
	}
	if vm.Spec.Requester.Did != "" && vm.Spec.Requester.Did != payload.RequesterDID {
		return errors.New("bidder: the inbound event is not from the contract requester")
	}
	observedAt := service.nowUTC().Format(time.RFC3339)
	release := v1alpha1.ComputeVMRelease{RequestedAt: observedAt}
	if event.Payload.URI != "" {
		release.Request = &commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID}
	}
	if err := service.vms.Release(ctx, vm.Name, release); err != nil {
		return fmt.Errorf("bidder: recording the release request on the ComputeVM: %w", err)
	}
	bid, err := service.bidForVM(ctx, vm)
	if err != nil {
		return err
	}
	info := contractInfo{computeVM: vm.Name, receiptURI: payload.Receipt.URI, receiptCID: payload.Receipt.CID,
		acceptAuthor: payload.RequesterDID}
	if contract := vm.Spec.Contract; contract != nil {
		info.acceptURI, info.acceptCID = contract.Accept.URI, contract.Accept.CID
		info.submitEventURL = contract.SubmitEventURL
	}
	if bid != nil {
		info.bidName = bid.Name
	}
	service.markBidReleaseRequested(ctx, bid, info,
		&commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID},
		&commonv1.StrongRef{URI: payload.RequestEvent.URI, CID: payload.RequestEvent.CID},
		observedAt)
	return nil
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
	adapter := &marketEventAdapter{service: s, records: s.records, did: s.did, attestation: s.attestation, call: s.callService, log: s.log}
	if err := registry.Register(adapter); err != nil {
		return err
	}
	provision := &provisionAdapter{service: s, log: s.log}
	if err := registry.Register(provision); err != nil {
		return err
	}
	release := &releaseAdapter{service: s}
	if err := registry.Register(release); err != nil {
		return err
	}
	inbox := &marketInboxAdapter{service: s, log: s.log}
	if err := registry.Register(inbox); err != nil {
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
	if contract.Receipt.URI == "" || contract.Receipt.CID == "" {
		return "", errors.New("bidder: a report StateSync needs a receipt")
	}
	if vm.UID == "" {
		return "", errors.New("bidder: a report StateSync needs a ComputeVM UID")
	}
	payload := map[string]any{
		"collection": collection,
		"receipt":    map[string]any{"uri": contract.Receipt.URI, "cid": contract.Receipt.CID},
		"record":     record,
	}
	if contract.SubmitEventURL != "" {
		payload["submitEventUrl"] = contract.SubmitEventURL
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
			"idempotencyKey": reportSyncKey(collection, contract.Receipt.URI),
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

func (s *Service) ensureTerminationSync(ctx context.Context, info contractInfo, reason, observedAt string, request *commonv1.StrongRef) (string, error) {
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
	if contract := vm.Spec.Contract; contract == nil || contract.Receipt.URI == "" || contract.SubmitEventURL == "" {
		submitEventURL := info.submitEventURL
		vm = vm.DeepCopy()
		if vm.Spec.Contract == nil {
			vm.Spec.Contract = &v1alpha1.ComputeVMContract{}
		}
		if vm.Spec.Contract.Receipt.URI == "" {
			vm.Spec.Contract.Receipt.URI = info.receiptURI
			vm.Spec.Contract.Receipt.CID = info.receiptCID
		}
		vm.Spec.Contract.SubmitEventURL = cmp.Or(vm.Spec.Contract.SubmitEventURL, submitEventURL)
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
		PayloadURI: payloadURI,
		PayloadCid: payloadCID,
		EventURI:   eventURI,
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

func (s *Service) ensureInboundTerminationSync(ctx context.Context, vm *v1alpha1.ComputeVM, request *commonv1.StrongRef, requesterDID, eventURI, eventCID string, record map[string]any) error {
	if vm == nil || vm.UID == "" {
		return errors.New("bidder: an inbound termination StateSync needs a ComputeVM")
	}
	observedAt := s.nowUTC().Format(time.RFC3339)
	object, err := computevm.TerminationStateSync(vm, s.cfg.Workspace, "deleted_on_request", observedAt, request)
	if err != nil {
		return err
	}
	spec, ok := object.Object["spec"].(map[string]any)
	if !ok {
		return errors.New("bidder: the termination StateSync has no spec")
	}
	payloadMap, ok := spec["payload"].(map[string]any)
	if !ok {
		return errors.New("bidder: the termination StateSync has no payload")
	}
	payloadMap["requestEvent"] = map[string]any{"uri": eventURI, "cid": eventCID, "record": record}
	payloadMap["requesterDid"] = requesterDID
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return err
	}
	name := object.GetName()
	if _, err := dyn.Resource(statesync.StateSyncGVR).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("bidder: creating the termination StateSync %s: %w", name, err)
		}
	} else {
		s.log.Info("a termination StateSync is created", "computevm", vm.Name, "statesync", name)
	}
	return nil
}

type releasePayload struct {
	ReceiptURI string `json:"receiptUri"`

	RequesterDID string `json:"requesterDid"`

	Event marketEventRequest `json:"event"`
}

func decodeReleasePayload(raw json.RawMessage) (releasePayload, error) {
	var payload releasePayload
	if len(raw) == 0 {
		return payload, errors.New("bidder: the release payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("bidder: the release payload is not JSON: %w", err)
	}
	switch {
	case strings.TrimSpace(payload.ReceiptURI) == "":
		return payload, errors.New("bidder: the release payload names no receipt")
	case strings.TrimSpace(payload.RequesterDID) == "":
		return payload, errors.New("bidder: the release payload names no requester")
	case strings.TrimSpace(payload.Event.URI) == "" || payload.Event.Record == nil:
		return payload, errors.New("bidder: the release payload names no event")
	}
	return payload, nil
}

func releaseSyncName(receiptURI, eventURI string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(receiptURI) + "\x00" + strings.TrimSpace(eventURI)))
	return releaseSyncPrefix + hex.EncodeToString(sum[:8])
}

func releaseAppliedKey(payload releasePayload) string {
	return "release:" + payload.ReceiptURI + "#" + payload.Event.URI
}

func (s *Service) ensureReleaseSync(ctx context.Context, receiptURI, requesterDID, eventURI, eventCID string, record map[string]any) error {
	payload := releasePayload{ReceiptURI: receiptURI, RequesterDID: requesterDID}
	payload.Event.URI, payload.Event.CID, payload.Event.Record = eventURI, eventCID, record
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return err
	}
	name := releaseSyncName(receiptURI, eventURI)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "StateSync",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"statesync.socialweb.computer/adapter": ReleaseSyncAdapter,
			},
		},
		"spec": map[string]any{
			"source":         map[string]any{"workspace": s.cfg.Workspace},
			"sink":           map[string]any{"adapter": ReleaseSyncAdapter},
			"direction":      string(v1alpha1.StateSyncDirectionOutbound),
			"payload":        stored,
			"idempotencyKey": "release:" + receiptURI + "#" + eventURI,
			"conflictPolicy": string(v1alpha1.StateSyncConflictSourceWins),
			"deletionPolicy": string(v1alpha1.StateSyncDeletionRetain),
		},
	}}
	dyn, err := s.syncDynamic(s.cfg.Workspace)
	if err != nil {
		return err
	}
	if _, err := dyn.Resource(statesync.StateSyncGVR).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("bidder: creating the release StateSync %s: %w", name, err)
		}
	} else {
		s.log.Info("a release StateSync is created", "receipt", receiptURI, "statesync", name)
	}
	return nil
}

func (s *Service) vmForAccept(ctx context.Context, acceptURI, acceptCID string) (*v1alpha1.ComputeVM, error) {
	if s.vms == nil {
		return nil, errors.New("bidder: the ComputeVM store is not configured")
	}
	key := computevm.AcceptRef{URI: acceptURI, CID: acceptCID}.Key()
	if key == "" {
		return nil, nil
	}
	list, err := s.vms.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if computevm.AcceptKeyOf(&list[i]) != key {
			continue
		}
		if list[i].DeletionTimestamp != nil || computevm.Ended(&list[i]) {
			continue
		}
		return &list[i], nil
	}
	return nil, nil
}

func (s *Service) bidForVM(ctx context.Context, vm *v1alpha1.ComputeVM) (*v1alpha1.Bid, error) {
	if s.bids == nil || vm == nil {
		return nil, nil
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range bids {
		if vm.Name != "" && bids[i].Status.ComputeVM == vm.Name {
			return &bids[i], nil
		}
	}
	if contract := vm.Spec.Contract; contract != nil {
		for i := range bids {
			accept := bids[i].Status.Accept
			if accept != nil && accept.URI == contract.Accept.URI && accept.CID == contract.Accept.CID {
				return &bids[i], nil
			}
		}
	}
	return nil, nil
}

func (s *Service) bidForAcceptStatus(ctx context.Context, acceptURI, acceptCID string) (*v1alpha1.Bid, error) {
	if s.bids == nil {
		return nil, errors.New("bidder: the bidder has no Bid store")
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range bids {
		accept := bids[i].Status.Accept
		if accept != nil && accept.URI == acceptURI && accept.CID == acceptCID {
			return &bids[i], nil
		}
	}
	return nil, nil
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
	return s.bidForAcceptStatus(ctx, payload.Accept.URI, payload.Accept.CID)
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
	return contract.Accept.URI == payload.Accept.URI && contract.Accept.CID == payload.Accept.CID && contract.Receipt.URI != ""
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
		observation.Meta["receiptUri"] = contract.Receipt.URI
		observation.Meta["receiptCid"] = contract.Receipt.CID
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
		return provisionSettledObservation("terminated:"+bid.Status.Terminated.ReceiptURI, payload, request), nil
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
		if bid.Status.Accept == nil || bid.Status.Accept.URI != payload.Accept.URI || bid.Status.Accept.CID != payload.Accept.CID {
			return statesync.Observation{}, errors.New("bidder: the bid's contract was released")
		}
		return provisionSettledObservation("terminated:"+terminated.ReceiptURI, payload, request), nil
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
	receiptSubmitEvent := ""
	if service.cfg.inboundXRPC() {
		receiptSubmitEvent = accept.SubmitEvent
	}
	receiptURI, receiptCID, err := service.createReceipt(ctx, market.ReceiptRefs{
		RFP:         market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID},
		Bid:         market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID},
		Accept:      market.RecordRef{URI: payload.Accept.URI, CID: payload.Accept.CID},
		Payload:     &market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID},
		SubmitEvent: receiptSubmitEvent,
	}, acceptResolved, payload.RequesterDID)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: writing the receipt: %w", err)
	}
	status := cloneBidStatus(bid.Status)
	if status.Accept == nil || status.Accept.URI != payload.Accept.URI || status.Accept.CID != payload.Accept.CID {
		status.Accept = &commonv1.StrongRef{URI: payload.Accept.URI, CID: payload.Accept.CID}
		if status.Timings.AcceptedAt == "" {
			status.Timings.AcceptedAt = now.Format(time.RFC3339)
		}
		status.Phase = v1alpha1.BidPhaseWon
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
		Bid:            bidOwner(bid),
	})
	if err != nil {
		return statesync.Observation{}, err
	}
	contract := v1alpha1.ComputeVMContract{
		Receipt:        commonv1.StrongRef{URI: receiptURI, CID: receiptCID},
		Accept:         commonv1.StrongRef{URI: payload.Accept.URI, CID: payload.Accept.CID},
		SubmitEventURL: submitEventURL,
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
				release := v1alpha1.ComputeVMRelease{RequestedAt: terminated.At}
				if terminated.PayloadURI != "" {
					release.Request = &commonv1.StrongRef{URI: terminated.PayloadURI, CID: terminated.PayloadCid}
				}
				if err := service.vms.Release(ctx, result.Name, release); err != nil {
					return statesync.Observation{}, err
				}
				var requestRef *commonv1.StrongRef
				if terminated.PayloadURI != "" {
					requestRef = &commonv1.StrongRef{URI: terminated.PayloadURI, CID: terminated.PayloadCid}
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

type releaseAdapter struct {
	service *Service
}

func (a *releaseAdapter) Name() string { return ReleaseSyncAdapter }

func releaseSettledObservation(payload releasePayload, request statesync.Request, ref string) statesync.Observation {
	ref = cmp.Or(ref, "released:"+payload.ReceiptURI)
	return statesync.Observation{
		Exists: true,
		Ref:    ref,
		Hash:   provisionPayloadHash(request.Payload),
		At:     time.Now().UTC().Format(time.RFC3339),
	}
}

func (a *releaseAdapter) Observe(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, err := decodeReleasePayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	service := a.service
	if service == nil || service.records == nil || service.vms == nil || service.did == "" {
		return statesync.Observation{}, errors.New("bidder: the release adapter is not configured")
	}
	if _, ok := service.releaseApplied.Load(releaseAppliedKey(payload)); ok {
		return releaseSettledObservation(payload, request, ""), nil
	}
	if market.ATURIAuthority(payload.ReceiptURI) != service.did {
		return statesync.Observation{Exists: false}, nil
	}
	resolved, err := service.records.Resolve(ctx, market.RecordRef{URI: payload.ReceiptURI})
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return statesync.Observation{Exists: false}, nil
		}
		return statesync.Observation{}, err
	}
	receipt, err := market.ParseReceipt(resolved.Value)
	if err != nil {
		return statesync.Observation{}, err
	}
	vm, err := service.vmForAccept(ctx, receipt.Accept.URI, receipt.Accept.CID)
	if err != nil {
		return statesync.Observation{}, err
	}
	if vm != nil {
		if computevm.ReleaseRequested(vm) {
			return releaseSettledObservation(payload, request, vm.Name), nil
		}
		return statesync.Observation{Exists: false}, nil
	}
	bid, err := service.bidForAcceptStatus(ctx, receipt.Accept.URI, receipt.Accept.CID)
	if err != nil {
		return statesync.Observation{}, err
	}
	if bid != nil && bid.Status.Terminated != nil && bid.Status.Terminated.ReceiptURI == payload.ReceiptURI {
		return releaseSettledObservation(payload, request, ""), nil
	}
	return statesync.Observation{Exists: false}, nil
}

func (a *releaseAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	service := a.service
	if service == nil || service.records == nil || service.vms == nil || service.did == "" {
		return statesync.Observation{}, errors.New("bidder: the release adapter is not configured")
	}
	payload, err := decodeReleasePayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	if market.ATURIAuthority(payload.ReceiptURI) != service.did {
		return statesync.Observation{}, errors.New("bidder: the release intent names another bidder's receipt")
	}
	resolved, err := service.records.Resolve(ctx, market.RecordRef{URI: payload.ReceiptURI})
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: resolving the receipt of the release intent: %w", err)
	}
	receipt, err := market.ParseReceipt(resolved.Value)
	if err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: the receipt of the release intent is invalid: %w", err)
	}
	if market.ATURIAuthority(receipt.Accept.URI) != payload.RequesterDID {
		return statesync.Observation{}, errors.New("bidder: the release intent is not from the accept author")
	}
	event, err := verifyInboundDeleteEvent(ctx, service, payload.Event, payload.ReceiptURI, resolved.CID, payload.RequesterDID)
	if err != nil {
		return statesync.Observation{}, err
	}
	requestRef := &commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID}
	eventRef := &commonv1.StrongRef{URI: payload.Event.URI, CID: payload.Event.CID}
	observedAt := service.nowUTC().Format(time.RFC3339)
	info := contractInfo{receiptURI: payload.ReceiptURI, receiptCID: resolved.CID,
		acceptURI: receipt.Accept.URI, acceptCID: receipt.Accept.CID, acceptAuthor: payload.RequesterDID,
		submitEventURL: service.submitEventURL(ctx, receipt.SubmitEvent)}
	vm, err := service.vmForAccept(ctx, receipt.Accept.URI, receipt.Accept.CID)
	if err != nil {
		return statesync.Observation{}, err
	}
	if vm == nil {
		bid, err := service.bidForAcceptStatus(ctx, receipt.Accept.URI, receipt.Accept.CID)
		if err != nil {
			return statesync.Observation{}, err
		}
		if bid == nil {
			return statesync.Observation{}, errors.New("bidder: the release intent names no known contract")
		}
		info.bidName = bid.Name
		service.markBidReleaseRequested(ctx, bid, info, requestRef, eventRef, observedAt)
		service.contracts.Store(payload.ReceiptURI, info)
		service.releaseApplied.Store(releaseAppliedKey(payload), true)
		return releaseSettledObservation(payload, request, ""), nil
	}
	info.computeVM = vm.Name
	release := v1alpha1.ComputeVMRelease{RequestedAt: observedAt}
	if requestRef.URI != "" {
		release.Request = &commonv1.StrongRef{URI: requestRef.URI, CID: requestRef.CID}
	}
	if err := service.vms.Release(ctx, vm.Name, release); err != nil {
		return statesync.Observation{}, fmt.Errorf("bidder: recording the release request on the ComputeVM: %w", err)
	}
	bid, err := service.bidForVM(ctx, vm)
	if err != nil {
		return statesync.Observation{}, err
	}
	if bid != nil {
		info.bidName = bid.Name
	}
	service.markBidReleaseRequested(ctx, bid, info, requestRef, eventRef, observedAt)
	service.contracts.Store(payload.ReceiptURI, info)
	if _, err := service.ensureTerminationSync(ctx, info, "deleted_on_request", observedAt, requestRef); err != nil {
		return statesync.Observation{}, err
	}
	service.releaseApplied.Store(releaseAppliedKey(payload), true)
	return releaseSettledObservation(payload, request, vm.Name), nil
}

func (a *releaseAdapter) Delete(ctx context.Context, request statesync.Request) error {
	return nil
}

const MarketInboxAdapter = "bidder.market-inbox"

type marketInboxPayload struct {
	DID string `json:"did"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Collection string `json:"collection"`

	Record map[string]any `json:"record"`
}

func decodeMarketInboxPayload(raw json.RawMessage) (marketInboxPayload, error) {
	var payload marketInboxPayload
	if len(raw) == 0 {
		return payload, errors.New("bidder: the market inbox payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("bidder: the market inbox payload is not JSON: %w", err)
	}
	if strings.TrimSpace(payload.URI) == "" || strings.TrimSpace(payload.CID) == "" {
		return payload, errors.New("bidder: the market inbox payload names no record")
	}
	if strings.TrimSpace(payload.Collection) == "" {
		payload.Collection = market.NSIDFromURI(payload.URI)
	}
	return payload, nil
}

type marketInboxAdapter struct {
	service *Service

	log *slog.Logger
}

func (a *marketInboxAdapter) Name() string { return MarketInboxAdapter }

func (a *marketInboxAdapter) logf() *slog.Logger {
	if a == nil || a.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.log
}

func (a *marketInboxAdapter) Observe(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, err := decodeMarketInboxPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	service := a.service
	if service != nil && service.bids != nil && service.did != "" && payload.Collection == market.RFPNSID {
		bid, err := service.bids.Get(ctx, bidName(service.did, payload.URI, payload.CID))
		if err != nil {
			return statesync.Observation{}, err
		}
		if bid != nil {
			return statesync.InboxObservation(request, payload.URI), nil
		}
	}
	return statesync.Observation{At: time.Now().UTC().Format(time.RFC3339)}, nil
}

func (a *marketInboxAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, err := decodeMarketInboxPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	service := a.service
	if service == nil {
		return statesync.Observation{}, errors.New("bidder: the market inbox adapter has no service")
	}
	a.logf().Info("market inbox applied", "collection", payload.Collection, "uri", payload.URI, "bids", service.bids != nil, "records", service.records != nil, "did", service.did)
	switch payload.Collection {
	case market.RFPNSID:
		if service.bids == nil || service.records == nil || service.did == "" {
			return statesync.Observation{}, errors.New("bidder: the bidder is not ready to bid")
		}
		bid, err := service.enqueueBid(ctx, payload.URI, payload.CID, payload.DID)
		if err != nil {
			return statesync.Observation{}, fmt.Errorf("bidder: recording the bid intent: %w", err)
		}
		if bid != nil {
			a.logf().Info("market inbox enqueued a bid", "rfp", payload.URI, "bid", bid.Name, "phase", string(bid.Status.Phase))
		}
		service.kickBids()
	case market.AcceptNSID:
		if service.records == nil || service.bids == nil || service.vms == nil || service.syncController == nil {
			return statesync.Observation{}, errors.New("bidder: the bidder is not ready to accept")
		}
		provision := provisionPayload{RequesterDID: payload.DID}
		provision.Accept.URI, provision.Accept.CID = payload.URI, payload.CID
		if _, err := service.ensureProvisionSync(ctx, provision); err != nil {
			return statesync.Observation{}, fmt.Errorf("bidder: recording the provision intent: %w", err)
		}
		service.kickStateSync()
	case market.EventNSID:
		if err := service.applyMarketEvent(ctx, payload.URI, payload.CID, payload.Record, payload.DID); err != nil {
			return statesync.Observation{}, err
		}
	}
	return statesync.InboxObservation(request, payload.URI), nil
}

func (a *marketInboxAdapter) Delete(ctx context.Context, request statesync.Request) error {
	return nil
}

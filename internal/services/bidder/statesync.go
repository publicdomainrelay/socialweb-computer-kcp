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
	payloadRKey := strings.TrimSpace(p.PayloadRKey)
	if payloadRKey == "" {
		sum := sha256.Sum256([]byte(key))
		payloadRKey = "termination-payload-" + hex.EncodeToString(sum[:16])
	}
	eventRKey := strings.TrimSpace(p.EventRKey)
	if eventRKey == "" {
		sum := sha256.Sum256([]byte("event:" + key))
		eventRKey = "termination-event-" + hex.EncodeToString(sum[:16])
	}
	return payloadRKey, eventRKey
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

package requester

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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"
)

const (
	MarketInboxAdapter = "requester.market-inbox"

	requesterStateSyncInterval = 2 * time.Second
)

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
		return payload, errors.New("requester: the market inbox payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("requester: the market inbox payload is not JSON: %w", err)
	}
	if strings.TrimSpace(payload.URI) == "" || strings.TrimSpace(payload.CID) == "" {
		return payload, errors.New("requester: the market inbox payload names no record")
	}
	if strings.TrimSpace(payload.Collection) == "" {
		payload.Collection = market.NSIDFromURI(payload.URI)
	}
	return payload, nil
}

func marketInboxObservation(request statesync.Request, ref string) statesync.Observation {
	sum := sha256.Sum256(request.Payload)
	return statesync.Observation{
		Exists: true,
		Ref:    ref,
		Hash:   "sha256:" + hex.EncodeToString(sum[:]),
		At:     time.Now().UTC().Format(time.RFC3339),
	}
}

func isBidCollection(collection string) bool {
	switch collection {
	case market.BidNSID, market.BidsFreeNSID, market.BidsX402NSID:
		return true
	}
	return false
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
	if a.service != nil && a.service.inbox != nil && isBidCollection(payload.Collection) {
		if _, err := a.service.inbox.Get(ctx, ref.Ref{LogicalCluster: a.service.cfg.Workspace, Name: bidInboxName(payload.URI, payload.CID)}); err != nil {
			if apierrors.IsNotFound(err) {
				return statesync.Observation{At: time.Now().UTC().Format(time.RFC3339)}, nil
			}
			return statesync.Observation{}, err
		}
		return marketInboxObservation(request, payload.URI), nil
	}
	return marketInboxObservation(request, request.Key), nil
}

func (a *marketInboxAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	payload, err := decodeMarketInboxPayload(request.Payload)
	if err != nil {
		return statesync.Observation{}, err
	}
	service := a.service
	if service == nil {
		return statesync.Observation{}, errors.New("requester: the market inbox adapter has no service")
	}
	switch {
	case isBidCollection(payload.Collection):
		if err := service.storeBidMailbox(ctx, payload.DID, payload.URI, payload.CID, payload.Record); err != nil {
			return statesync.Observation{}, err
		}
	case payload.Collection == market.ReceiptNSID:
		event := v1alpha1.ComputeContractEvent{URI: payload.URI, CID: payload.CID, ReceiptURI: payload.URI, ReceivedAt: service.nowISO()}
		if err := service.storeInboxEvent(ctx, event); err != nil {
			return statesync.Observation{}, err
		}
	case payload.Collection == market.EventNSID:
		envelope, err := market.ParseEvent(payload.Record)
		if err != nil {
			return statesync.Observation{}, fmt.Errorf("requester: the inbox event is invalid: %w", err)
		}
		event := v1alpha1.ComputeContractEvent{URI: payload.URI, CID: payload.CID, ReceiptURI: envelope.Receipt.URI, ReceivedAt: service.nowISO()}
		if err := service.storeInboxEvent(ctx, event); err != nil {
			return statesync.Observation{}, err
		}
	}
	return marketInboxObservation(request, payload.URI), nil
}

func (a *marketInboxAdapter) Delete(ctx context.Context, request statesync.Request) error {
	return nil
}

type inboxNoopAdapter struct {
	name string
}

func (a *inboxNoopAdapter) Name() string { return a.name }

func (a *inboxNoopAdapter) Observe(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	return marketInboxObservation(request, request.Key), nil
}

func (a *inboxNoopAdapter) Apply(ctx context.Context, request statesync.Request) (statesync.Observation, error) {
	return marketInboxObservation(request, request.Key), nil
}

func (a *inboxNoopAdapter) Delete(ctx context.Context, request statesync.Request) error {
	return nil
}

func (s *Service) inboundXRPC() bool {
	return s.cfg.inboundXRPC()
}

func (s *Service) outboundXRPC() bool {
	return s.cfg.outboundXRPC()
}

func (s *Service) startStateSync() error {
	if s.cfg.Deps.KCP == nil {
		return errors.New("requester: the component has no kcp clients")
	}
	registry := statesync.NewRegistry()
	for _, adapter := range []statesync.Adapter{
		&marketInboxAdapter{service: s, log: s.log},
		&inboxNoopAdapter{name: BidInboxAdapter},
		&inboxNoopAdapter{name: InboxAdapter},
	} {
		if err := registry.Register(adapter); err != nil {
			return err
		}
	}
	controller, err := statesync.NewController(statesync.Options{
		Workspace: s.cfg.Workspace,
		Dynamic:   s.syncDynamic,
		Registry:  registry,
		Log:       s.log,
		Interval:  requesterStateSyncInterval,
	})
	if err != nil {
		return err
	}
	s.syncController = controller
	return nil
}

func (s *Service) syncDynamic(workspace string) (dynamic.Interface, error) {
	if s.cfg.Deps.KCP == nil {
		return nil, errors.New("requester: the component has no kcp clients")
	}
	client, err := s.cfg.Deps.KCP.Client(workspace)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(client.RESTConfig())
	if cfg == nil || strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("requester: the kcp client has no rest config")
	}
	if !strings.Contains(cfg.Host, "/clusters/") {
		cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + workspace
	}
	return dynamic.NewForConfig(cfg)
}

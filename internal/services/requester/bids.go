package requester

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

const (
	BidInboxAdapter = "requester.bid-inbox"

	bidInboxLabel = "socialweb.computer/bid-inbox"

	bidInboxPrefix = "bid-"

	bidVerifyTimeout = 10 * time.Second
)

type bidInboxPayload struct {
	DID string `json:"did"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Record map[string]any `json:"record"`
}

func bidInboxName(uri, cid string) string {
	sum := sha256.Sum256([]byte(uri + "#" + cid))
	return bidInboxPrefix + hex.EncodeToString(sum[:16])
}

func (s *Service) storeBidMailbox(ctx context.Context, did, uri, cid string, record map[string]any) error {
	if s.inbox == nil {
		return errors.New("requester: the bid inbox store is not configured")
	}
	if uri == "" || cid == "" {
		return errors.New("requester: the bid ref is incomplete")
	}
	payload, err := json.Marshal(bidInboxPayload{DID: did, URI: uri, CID: cid, Record: record})
	if err != nil {
		return err
	}
	object := &v1alpha1.StateSync{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "StateSync"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   bidInboxName(uri, cid),
			Labels: map[string]string{bidInboxLabel: "true"},
		},
		Spec: v1alpha1.StateSyncSpec{
			Sink:           v1alpha1.StateSyncSink{Adapter: BidInboxAdapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        payload,
			IdempotencyKey: "bid:" + uri + "#" + cid,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
	err = s.inbox.Create(ctx, s.workspace(), object)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (s *Service) processBidMailboxes(ctx context.Context) {
	if s.inbox == nil {
		return
	}
	items, err := s.inbox.List(ctx, s.workspace())
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("requester_bid_inbox_list_failed", "err", err)
		}
		return
	}
	opts := market.VerifyRecordOptions{
		KeysForDID:             market.NewDirectoryKeyLister(s.directory),
		KeyResolver:            market.DefaultKeyResolver,
		KeyBoundByBadgeBlueKey: s.badgeBlueBinding(),
	}
	for i := range items {
		mailbox := &items[i]
		if mailbox.Spec.Sink.Adapter != BidInboxAdapter {
			continue
		}
		var payload bidInboxPayload
		if err := json.Unmarshal(mailbox.Spec.Payload, &payload); err != nil || payload.URI == "" || payload.CID == "" {
			s.log.Warn("requester_bid_inbox_invalid", "statesync", mailbox.Name)
			s.deleteBidMailbox(ctx, mailbox.Name)
			continue
		}
		if payload.DID == "" || payload.DID != market.ATURIAuthority(payload.URI) {
			s.log.Warn("requester_bid_authority_mismatch", "statesync", mailbox.Name, "bid", payload.URI, "bidder", payload.DID)
			s.deleteBidMailbox(ctx, mailbox.Name)
			continue
		}
		bid, err := market.ParseBid(payload.Record)
		if err != nil {
			s.log.Warn("requester_bid_inbox_invalid", "statesync", mailbox.Name, "bid", payload.URI, "err", err)
			s.deleteBidMailbox(ctx, mailbox.Name)
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, bidVerifyTimeout)
		valid := market.VerifyRecordSignatures(vctx, payload.Record, market.ATURIAuthority(payload.URI), opts)
		verr := vctx.Err()
		cancel()
		if verr != nil {
			s.log.Warn("requester_bid_verify_unavailable", "statesync", mailbox.Name, "bid", payload.URI, "err", verr)
			continue
		}
		if !valid {
			s.log.Warn("requester_bid_refused", "statesync", mailbox.Name, "bid", payload.URI, "bidder", payload.DID)
			s.deleteBidMailbox(ctx, mailbox.Name)
			continue
		}
		s.queueBid(bid.RFP.URI, BidRecord{DID: payload.DID, URI: payload.URI, CID: payload.CID, Record: payload.Record, ArrivedAt: s.now()})
		s.log.Info("requester_bid_recovered", "bidder", payload.DID, "uri", payload.URI, "rfp", bid.RFP.URI)
		s.deleteBidMailbox(ctx, mailbox.Name)
	}
}

func (s *Service) deleteBidMailbox(ctx context.Context, name string) {
	if s.inbox == nil {
		return
	}
	if err := s.inbox.Delete(ctx, ref.New(s.workspace(), "", name)); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("requester_bid_inbox_delete_failed", "statesync", name, "err", err)
		}
	}
}

package guestidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

const (
	RBACCollection = "com.fedproxy.rbac"

	maximumRBACPages = 100

	maximumRBACRecords = 5000
)

var RBACAtAcceptSince = time.Date(2026, time.October, 30, 21, 18, 3, 0, time.UTC)

type RepoRBAC struct {
	Reader atprotorepo.RecordReader

	Log *slog.Logger
}

func (h *RepoRBAC) Records(ctx context.Context, requesterDID, acceptURI string, acceptedAt time.Time) ([]RBACRecord, error) {
	if h == nil || h.Reader == nil {
		return nil, errors.New("guestidentity: the RBAC reader has no record reader")
	}
	log := h.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if key := market.RBACRKeyOfAccept(acceptURI); key != "" {
		record, err := h.Reader.GetRecord(ctx, requesterDID, RBACCollection, key)
		switch {
		case err == nil:
			var rb RBACRecord
			if err := json.Unmarshal(record.Value, &rb); err != nil {
				log.Warn("a requester RBAC record is not one and is skipped", "requester", requesterDID, "rkey", key, "err", err.Error())
				return []RBACRecord{}, nil
			}
			return []RBACRecord{rb}, nil
		case !errors.Is(err, atprotorepo.ErrNotFound):
			return nil, fmt.Errorf("guestidentity: reading the RBAC record %s of %s: %w", key, requesterDID, err)
		}
	}
	if acceptedAt.IsZero() || !acceptedAt.Before(RBACAtAcceptSince) {
		return []RBACRecord{}, nil
	}
	return h.listed(ctx, requesterDID, log)
}

func (h *RepoRBAC) listed(ctx context.Context, requesterDID string, log *slog.Logger) ([]RBACRecord, error) {
	out := []RBACRecord{}
	cursor := ""
	seen := map[string]bool{}
	for range maximumRBACPages {
		page, err := h.Reader.ListRecords(ctx, requesterDID, RBACCollection, atprotorepo.ListOptions{Limit: 100, Cursor: cursor})
		if err != nil && cursor == "" && errors.Is(err, atprotorepo.ErrNotFound) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("guestidentity: listing the RBAC records of %s: %w", requesterDID, err)
		}
		for _, record := range page.Records {
			var rb RBACRecord
			if err := json.Unmarshal(record.Value, &rb); err != nil {
				log.Warn("a requester RBAC record is not one and is skipped", "requester", requesterDID, "err", err.Error())
				continue
			}
			out = append(out, rb)
			if len(out) >= maximumRBACRecords {
				return out, nil
			}
		}
		if page.Cursor == "" {
			return out, nil
		}
		if seen[page.Cursor] {
			return nil, fmt.Errorf("guestidentity: the RBAC records of %s repeat cursor %q", requesterDID, page.Cursor)
		}
		seen[page.Cursor] = true
		cursor = page.Cursor
	}
	return nil, fmt.Errorf("guestidentity: the RBAC records of %s run past %d pages", requesterDID, maximumRBACPages)
}

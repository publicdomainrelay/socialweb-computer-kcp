package guestidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

const (
	RBACCollection = "com.fedproxy.rbac"

	maximumRBACPages = 100

	maximumRBACRecords = 5000
)

type RepoRBAC struct {
	Reader atprotorepo.RecordReader

	Log *slog.Logger
}

func (h *RepoRBAC) Records(ctx context.Context, requesterDID string) ([]RBACRecord, error) {
	if h == nil || h.Reader == nil {
		return nil, errors.New("guestidentity: the RBAC reader has no record reader")
	}
	log := h.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	out := []RBACRecord{}
	cursor := ""
	seen := map[string]bool{}
	for range maximumRBACPages {
		page, err := h.Reader.ListRecords(ctx, requesterDID, RBACCollection, atprotorepo.ListOptions{Limit: 100, Cursor: cursor})
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

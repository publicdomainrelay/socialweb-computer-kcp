package infra

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	record "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
)

type IndexEntry struct {
	Collection string

	Rkey string

	CreatedAt time.Time

	ExpiresAt time.Time
}

func (e IndexEntry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

func IndexURI(did string) string {
	return "at://" + did + "/" + record.InfraNSID + "/" + ReservedRkey
}

func DecodeIndex(raw json.RawMessage) ([]IndexEntry, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, fmt.Errorf("%w: the index record is not a JSON object", ErrInvalidDeclaration)
	}
	r, err := record.ParseInfra(value)
	if err != nil {
		return nil, err
	}
	if r.Index == nil {
		return nil, fmt.Errorf("%w: the record at rkey %s holds no index", ErrInvalidDeclaration, ReservedRkey)
	}
	out := make([]IndexEntry, 0, len(r.Index.Live))
	for _, e := range r.Index.Live {
		rkey := strings.TrimSpace(e.Rkey)
		if rkey == "" || rkey == ReservedRkey {
			continue
		}
		entry := IndexEntry{Collection: strings.TrimSpace(e.Collection), Rkey: rkey}
		entry.CreatedAt, _ = time.Parse(time.RFC3339, e.CreatedAt)
		entry.ExpiresAt, _ = time.Parse(time.RFC3339, e.ExpiresAt)
		out = append(out, entry)
	}
	return out, nil
}

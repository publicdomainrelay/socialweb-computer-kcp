package infra

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	record "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
)

const (
	IndexMaxLive = 256

	IndexMaxEnded = 32

	IndexGoneAfter = 10 * time.Minute

	IndexDescription = "index of live deployments; not a deployment"

	IndexEndDeadline = "deadline"

	IndexEndDeleted = "deleted"
)

var (
	ErrIndexFull = fmt.Errorf("infra: the index holds %d live entries; nothing is added until one is pruned", IndexMaxLive)

	ErrNotIndex = errors.New("infra: the record at rkey self is not an index of live deployments")
)

type IndexKey struct {
	Collection string

	Rkey string
}

type IndexEntry struct {
	Collection string

	Rkey string

	CreatedAt time.Time

	ExpiresAt time.Time

	AddedAt time.Time

	Writer string
}

func (e IndexEntry) Key() IndexKey {
	return IndexKey{Collection: e.Collection, Rkey: e.Rkey}
}

func (e IndexEntry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

type IndexEnded struct {
	Collection string

	Rkey string

	EndedAt time.Time

	EndReason string
}

func (e IndexEnded) Key() IndexKey {
	return IndexKey{Collection: e.Collection, Rkey: e.Rkey}
}

type IndexDead struct {
	IndexEnded

	Gone bool
}

type IndexChange struct {
	Add *IndexEntry

	Remove *IndexEnded

	Dead []IndexDead
}

func IndexURI(did string) string {
	return "at://" + did + "/" + record.InfraNSID + "/" + ReservedRkey
}

func parseIndex(raw json.RawMessage) (*record.Infra, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, fmt.Errorf("%w: the index record is not a JSON object", ErrNotIndex)
	}
	r, err := record.ParseInfra(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotIndex, err)
	}
	if r.Index == nil {
		return nil, ErrNotIndex
	}
	return r, nil
}

func entryOf(e record.InfraIndexEntry) IndexEntry {
	out := IndexEntry{Collection: strings.TrimSpace(e.Collection), Rkey: strings.TrimSpace(e.Rkey), Writer: e.Writer}
	out.CreatedAt, _ = time.Parse(time.RFC3339, e.CreatedAt)
	out.ExpiresAt, _ = time.Parse(time.RFC3339, e.ExpiresAt)
	out.AddedAt, _ = time.Parse(time.RFC3339, e.AddedAt)
	return out
}

func DecodeIndex(raw json.RawMessage) ([]IndexEntry, error) {
	r, err := parseIndex(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDeclaration, err)
	}
	out := make([]IndexEntry, 0, len(r.Index.Live))
	for _, e := range r.Index.Live {
		entry := entryOf(e)
		if entry.Rkey == "" || entry.Rkey == ReservedRkey {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func IndexHolds(entries []IndexEntry, key IndexKey) bool {
	return slices.ContainsFunc(entries, func(e IndexEntry) bool { return e.Key() == key })
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func endedRecord(e IndexEnded, now time.Time) record.InfraIndexEnded {
	return record.InfraIndexEnded{Collection: e.Collection, Rkey: e.Rkey, EndedAt: stamp(cmp.Or(e.EndedAt, now)), EndReason: e.EndReason}
}

func ApplyIndex(raw json.RawMessage, ch IndexChange, now time.Time) (json.RawMessage, bool, error) {
	now = now.UTC()
	var r record.Infra
	changed := false
	if raw == nil {
		r = record.Infra{Type: record.InfraNSID, CreatedAt: stamp(now), Description: IndexDescription, Index: &record.InfraIndex{}}
	} else {
		parsed, err := parseIndex(raw)
		if err != nil {
			return nil, false, err
		}
		r = *parsed
	}
	dead := map[IndexKey]IndexDead{}
	for _, d := range ch.Dead {
		dead[d.Key()] = d
	}
	var ended []record.InfraIndexEnded
	var live []record.InfraIndexEntry
	for _, e := range r.Index.Live {
		entry := entryOf(e)
		key := entry.Key()
		d, isDead := dead[key]
		switch {
		case ch.Remove != nil && key == ch.Remove.Key():
			ended = append(ended, endedRecord(*ch.Remove, now))
		case entry.Expired(now):
			ended = append(ended, endedRecord(IndexEnded{Collection: key.Collection, Rkey: key.Rkey, EndedAt: entry.ExpiresAt, EndReason: IndexEndDeadline}, now))
		case isDead && !d.Gone:
			ended = append(ended, endedRecord(d.IndexEnded, now))
		case isDead && !entry.AddedAt.IsZero() && now.Sub(entry.AddedAt) >= IndexGoneAfter:
			ended = append(ended, endedRecord(IndexEnded{Collection: key.Collection, Rkey: key.Rkey, EndedAt: cmp.Or(d.EndedAt, now),
				EndReason: IndexEndDeleted}, now))
		default:
			live = append(live, e)
			continue
		}
		changed = true
	}
	if ch.Add != nil && !slices.ContainsFunc(live, func(e record.InfraIndexEntry) bool { return entryOf(e).Key() == ch.Add.Key() }) {
		if len(live) >= IndexMaxLive {
			return nil, false, ErrIndexFull
		}
		add := *ch.Add
		live = append(live, record.InfraIndexEntry{Collection: add.Collection, Rkey: add.Rkey, CreatedAt: stamp(cmp.Or(add.CreatedAt, now)),
			ExpiresAt: stamp(add.ExpiresAt), AddedAt: stamp(now), Writer: add.Writer})
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	slices.SortStableFunc(ended, func(a, b record.InfraIndexEnded) int { return strings.Compare(b.EndedAt, a.EndedAt) })
	recent := ended
	for _, e := range r.Index.RecentEnded {
		key := IndexKey{Collection: strings.TrimSpace(e.Collection), Rkey: strings.TrimSpace(e.Rkey)}
		if slices.ContainsFunc(ended, func(n record.InfraIndexEnded) bool { return n.Collection == key.Collection && n.Rkey == key.Rkey }) {
			continue
		}
		recent = append(recent, e)
	}
	if len(recent) > IndexMaxEnded {
		recent = recent[:IndexMaxEnded]
	}
	index := *r.Index
	index.Live, index.RecentEnded, index.UpdatedAt = live, recent, stamp(now)
	r.Index = &index
	out, err := json.Marshal(r)
	if err != nil {
		return nil, false, fmt.Errorf("infra: encoding the index: %w", err)
	}
	return out, true, nil
}

package infra

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

	recordKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,512}$`)
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

	Refresh bool

	Remove *IndexEnded

	Dead []IndexDead
}

type IndexView struct {
	Live []IndexEntry

	Ended []IndexKey
}

func IndexURI(did string) string {
	return "at://" + did + "/" + record.InfraNSID + "/" + ReservedRkey
}

func validKey(key IndexKey) bool {
	if key.Collection != record.InfraNSID && key.Collection != record.EphemeralInfraNSID {
		return false
	}
	return key.Rkey != ReservedRkey && key.Rkey != "." && key.Rkey != ".." && recordKeyPattern.MatchString(key.Rkey)
}

func salvage[T any](raw any) []T {
	items, _ := raw.([]any)
	var out []T
	for _, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var v T
		if json.Unmarshal(b, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

func parseIndex(raw json.RawMessage) (*record.Infra, bool, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, false, fmt.Errorf("%w: the index record is not a JSON object", ErrNotIndex)
	}
	r, err := record.ParseInfra(value)
	switch {
	case err == nil && r.Index != nil:
		return r, false, nil
	case err == nil:
		return nil, false, ErrNotIndex
	}
	index, ok := value["index"].(map[string]any)
	if typ, _ := value["$type"].(string); !ok || (typ != "" && typ != record.InfraNSID) {
		return nil, false, fmt.Errorf("%w: %v", ErrNotIndex, err)
	}
	created, _ := value["createdAt"].(string)
	if _, perr := time.Parse(time.RFC3339, created); perr != nil {
		created = ""
	}
	return &record.Infra{Type: record.InfraNSID, CreatedAt: created, Description: IndexDescription, Index: &record.InfraIndex{
		Live: salvage[record.InfraIndexEntry](index["live"]), RecentEnded: salvage[record.InfraIndexEnded](index["recentEnded"])}}, true, nil
}

func entryOf(e record.InfraIndexEntry) IndexEntry {
	out := IndexEntry{Collection: strings.TrimSpace(e.Collection), Rkey: strings.TrimSpace(e.Rkey), Writer: e.Writer}
	out.CreatedAt, _ = time.Parse(time.RFC3339, e.CreatedAt)
	out.ExpiresAt, _ = time.Parse(time.RFC3339, e.ExpiresAt)
	out.AddedAt, _ = time.Parse(time.RFC3339, e.AddedAt)
	return out
}

func endedKey(e record.InfraIndexEnded) IndexKey {
	return IndexKey{Collection: strings.TrimSpace(e.Collection), Rkey: strings.TrimSpace(e.Rkey)}
}

func DecodeIndexView(raw json.RawMessage) (IndexView, error) {
	r, _, err := parseIndex(raw)
	if err != nil {
		return IndexView{}, fmt.Errorf("%w: %v", ErrInvalidDeclaration, err)
	}
	var out IndexView
	for _, e := range r.Index.Live {
		if entry := entryOf(e); validKey(entry.Key()) {
			out.Live = append(out.Live, entry)
		}
	}
	for _, e := range r.Index.RecentEnded {
		out.Ended = append(out.Ended, endedKey(e))
	}
	return out, nil
}

func DecodeIndex(raw json.RawMessage) ([]IndexEntry, error) {
	view, err := DecodeIndexView(raw)
	return view.Live, err
}

func IndexHolds(entries []IndexEntry, key IndexKey) bool {
	return slices.ContainsFunc(entries, func(e IndexEntry) bool { return e.Key() == key })
}

func sameEntry(have IndexEntry, want IndexEntry) bool {
	return have.ExpiresAt.Equal(want.ExpiresAt) && (want.CreatedAt.IsZero() || have.CreatedAt.Equal(want.CreatedAt))
}

func (v IndexView) Settled(ch IndexChange) bool {
	switch {
	case ch.Add != nil:
		i := slices.IndexFunc(v.Live, func(e IndexEntry) bool { return e.Key() == ch.Add.Key() })
		return i >= 0 && (!ch.Refresh || sameEntry(v.Live[i], *ch.Add)) && !slices.Contains(v.Ended, ch.Add.Key())
	case ch.Remove != nil:
		return !IndexHolds(v.Live, ch.Remove.Key())
	}
	return true
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
		parsed, malformed, err := parseIndex(raw)
		if err != nil {
			return nil, false, err
		}
		r, changed = *parsed, malformed
		if r.CreatedAt == "" {
			r.CreatedAt, changed = stamp(now), true
		}
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
		case !validKey(key) || entry.AddedAt.IsZero() || entry.CreatedAt.IsZero():
		case ch.Remove != nil && key == ch.Remove.Key():
			ended = append(ended, endedRecord(*ch.Remove, now))
		case ch.Add != nil && key == ch.Add.Key():
			if ch.Refresh && !sameEntry(entry, *ch.Add) {
				e.ExpiresAt = stamp(ch.Add.ExpiresAt)
				if !ch.Add.CreatedAt.IsZero() {
					e.CreatedAt = stamp(ch.Add.CreatedAt)
				}
				changed = true
			}
			live = append(live, e)
			continue
		case entry.Expired(now):
			ended = append(ended, endedRecord(IndexEnded{Collection: key.Collection, Rkey: key.Rkey, EndedAt: entry.ExpiresAt, EndReason: IndexEndDeadline}, now))
		case isDead && !d.Gone:
			ended = append(ended, endedRecord(d.IndexEnded, now))
		case isDead && now.Sub(entry.AddedAt) >= IndexGoneAfter:
			ended = append(ended, endedRecord(IndexEnded{Collection: key.Collection, Rkey: key.Rkey, EndedAt: cmp.Or(d.EndedAt, now),
				EndReason: IndexEndDeleted}, now))
		default:
			live = append(live, e)
			continue
		}
		changed = true
	}
	var full error
	if ch.Add != nil && !slices.ContainsFunc(live, func(e record.InfraIndexEntry) bool { return entryOf(e).Key() == ch.Add.Key() }) {
		if len(live) >= IndexMaxLive {
			full = ErrIndexFull
		} else {
			add := *ch.Add
			live = append(live, record.InfraIndexEntry{Collection: add.Collection, Rkey: add.Rkey, CreatedAt: stamp(cmp.Or(add.CreatedAt, now)),
				ExpiresAt: stamp(add.ExpiresAt), AddedAt: stamp(now), Writer: add.Writer})
			changed = true
		}
	}
	slices.SortStableFunc(ended, func(a, b record.InfraIndexEnded) int { return strings.Compare(b.EndedAt, a.EndedAt) })
	recent := ended
	for _, e := range r.Index.RecentEnded {
		key := endedKey(e)
		switch {
		case slices.ContainsFunc(ended, func(n record.InfraIndexEnded) bool { return endedKey(n) == key }):
		case full == nil && ch.Add != nil && key == ch.Add.Key():
			changed = true
		default:
			recent = append(recent, e)
		}
	}
	if len(recent) > IndexMaxEnded {
		recent, changed = recent[:IndexMaxEnded], true
	}
	if !changed {
		return raw, false, full
	}
	index := *r.Index
	index.Live, index.RecentEnded, index.UpdatedAt = live, recent, stamp(now)
	r.Index = &index
	out, err := json.Marshal(r)
	if err != nil {
		return nil, false, fmt.Errorf("infra: encoding the index: %w", err)
	}
	return out, true, full
}

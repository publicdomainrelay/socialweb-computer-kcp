package events

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

const (
	CapabilitySubscribe = "events.subscribe"

	CapabilityDeliver = "events.deliver"

	HandlerKind = "EventHandler"

	RecordWatchKind = "AtprotoRecordWatch"

	ScopeAll = "*"
)

var ErrFutureCursor = errors.New("events: the cursor is past the source's newest event")

type Op struct {
	Action string

	Path string

	CID string

	Prev string
}

type Event struct {
	Source string

	Seq int64

	Repo string

	Commit string

	Rev string

	Time time.Time

	Ops []Op

	ServerFilter *ServerFilter
}

type ServerFilterSupport string

const (
	ServerFilterSupported ServerFilterSupport = "true"

	ServerFilterUnsupported ServerFilterSupport = "false"

	ServerFilterUnknown ServerFilterSupport = "unknown"

	MaxServerFilterCollections = 100

	MaxServerFilterDIDs = 10000

	WantedCollectionsParam = "wantedCollections"

	WantedDIDsParam = "wantedDids"

	ServerFilterInfoName = "ServerFilter"

	collectionWildcard = ".*"

	logDIDs = 10
)

func MatchCollection(patterns []string, collection string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if prefix, ok := strings.CutSuffix(p, collectionWildcard); ok {
			if strings.HasPrefix(collection, prefix+".") {
				return true
			}
			continue
		}
		if p == collection {
			return true
		}
	}
	return false
}

type ServerFilter struct {
	Collections []string

	DIDs []string

	Supported ServerFilterSupport
}

func (f *ServerFilter) Gapless() bool {
	return f != nil && f.Supported == ServerFilterSupported
}

type Filter struct {
	Collections []string

	DIDs []string

	Cursor *int64
}

type Stream interface {
	Next(ctx context.Context) (Event, error)

	Close() error
}

type SavedCursor struct {
	Cursor int64

	SavedAt time.Time
}

type CursorStore interface {
	Load(workspace, name, uid string) (SavedCursor, bool)

	Save(workspace, name, uid string, cursor int64) error
}

type Source interface {
	Subscribe(ctx context.Context, filter Filter) (Stream, error)
}

type Sink interface {
	Deliver(ctx context.Context, event Event) error
}

func (f Filter) Scope() []string {
	var out []string
	for _, v := range append(append([]string(nil), f.Collections...), f.DIDs...) {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return []string{ScopeAll}
	}
	return out
}

func (f Filter) ServerParams() (collections, dids []string) {
	collections, dids = trimmedUnique(f.Collections), trimmedUnique(f.DIDs)
	if len(collections) > MaxServerFilterCollections {
		collections = nil
	}
	if len(dids) > MaxServerFilterDIDs {
		dids = nil
	}
	return collections, dids
}

func LogDIDs(dids []string) []string {
	if len(dids) > logDIDs {
		return dids[:logDIDs]
	}
	return dids
}

func trimmedUnique(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (f Filter) Narrow(e Event) Event {
	if len(f.DIDs) > 0 && !slices.ContainsFunc(f.DIDs, func(d string) bool { return strings.TrimSpace(d) == e.Repo }) {
		e.Ops = nil
		return e
	}
	if len(f.Collections) == 0 {
		return e
	}
	kept := make([]Op, 0, len(e.Ops))
	for _, op := range e.Ops {
		collection, _, _ := strings.Cut(op.Path, "/")
		if MatchCollection(f.Collections, collection) {
			kept = append(kept, op)
		}
	}
	e.Ops = kept
	return e
}

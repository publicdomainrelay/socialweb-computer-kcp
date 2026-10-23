package events

import (
	"context"
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
		if slices.ContainsFunc(f.Collections, func(c string) bool { return strings.TrimSpace(c) == collection }) {
			kept = append(kept, op)
		}
	}
	e.Ops = kept
	return e
}

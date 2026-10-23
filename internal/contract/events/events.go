package events

import (
	"context"
	"time"
)

const (
	CapabilitySubscribe = "events.subscribe"

	CapabilityDeliver = "events.deliver"
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

type Source interface {
	Subscribe(ctx context.Context, filter Filter, fn func(Event) error) error
}

type Sink interface {
	Deliver(ctx context.Context, event Event) error
}

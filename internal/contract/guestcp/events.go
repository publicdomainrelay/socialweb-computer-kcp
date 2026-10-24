package guestcp

import (
	"context"
	"encoding/json"
	"errors"
)

const (
	CapabilityEvents = "guestcp.events"

	CapabilityQueue = "guestcp.queue"
)

var (
	ErrQueueNotOpen = errors.New("guestcp: the guest event queue is not open")

	ErrQueueFull = errors.New("guestcp: the guest event queue holds too many unacknowledged events")

	ErrQueueGone = errors.New("guestcp: the guest event queue is gone")

	ErrFutureCursor = errors.New("guestcp: the cursor is past the last event")

	ErrUnauthenticated = errors.New("guestcp: the guest bearer token is missing or refused")

	ErrUnknownGuest = errors.New("guestcp: no live ComputeVM holds this guest")

	ErrNoPublisher = errors.New("guestcp: the bidder does not publish guest events yet")
)

type QueuedEvent struct {
	Seq int64 `json:"seq"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Record json.RawMessage `json:"record,omitempty"`
}

type GuestEvent struct {
	ComputeVM string

	Collection string

	Tag string

	Kind string

	Record json.RawMessage
}

type Publish func(ctx context.Context, event GuestEvent) error

type Events interface {
	Append(ctx context.Context, computeVM string, event QueuedEvent) (bool, error)

	Serve(publish Publish) (cancel func())
}

type EventStream interface {
	Next(ctx context.Context) (QueuedEvent, error)

	Close()
}

type Queue interface {
	Open(dir string) error

	Retain(computeVMs []string)

	Subscribe(computeVM string, cursor *int64) (EventStream, error)

	Publish(ctx context.Context, event GuestEvent) error
}

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
)

type QueuedEvent struct {
	Seq int64 `json:"seq"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Record json.RawMessage `json:"record,omitempty"`
}

type Events interface {
	Append(ctx context.Context, computeVM string, event QueuedEvent) (bool, error)
}

type Queue interface {
	Open(dir string) error

	Retain(computeVMs []string)
}

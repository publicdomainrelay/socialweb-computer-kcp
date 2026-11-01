package guestcp

import (
	"context"
	"encoding/json"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	CapabilityInputs = "guestcp.inputs"

	CapabilityGuests = "guestcp.guests"
)

type Inputs struct {
	DID string

	Issuer string

	OpenBao *v1alpha1.OpenBaoServerStatus

	ComputeVMs []v1alpha1.ComputeVM
}

type Source interface {
	Inputs(ctx context.Context) (Inputs, error)

	Watch(fn func(Inputs)) (cancel func())
}

type Guests interface {
	RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error
}

type Reply struct {
	Status int

	Body json.RawMessage
}

type EventsRequest struct {
	AcceptURI string

	AcceptCID string

	Record map[string]any
}

type Plane interface {
	LoginStep(ctx context.Context, step string, input map[string]any) Reply

	Discovery(ctx context.Context) Reply

	Keys(ctx context.Context) Reply

	Events(ctx context.Context, authorization string, request EventsRequest) Reply

	Authenticate(ctx context.Context, authorization string) (string, error)
}

type Relay interface {
	URL() string

	Close()
}

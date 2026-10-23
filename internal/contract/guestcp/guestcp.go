package guestcp

import (
	"context"
	"encoding/json"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	CapabilityInputs = "guestcp.inputs"

	CapabilityGuests = "guestcp.guests"

	CapabilityGrants = "guestcp.grants"
)

type Inputs struct {
	DID string

	Issuer string

	OpenBao *v1alpha1.OpenBaoServerStatus

	ComputeVMs []v1alpha1.ComputeVM

	Grants []v1alpha1.GuestIdentityGrant
}

type Source interface {
	Inputs(ctx context.Context) (Inputs, error)

	Watch(fn func(Inputs)) (cancel func())
}

type Guests interface {
	RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error
}

type Grants interface {
	Apply(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error

	Delete(ctx context.Context, name string) error
}

type Reply struct {
	Status int

	Body json.RawMessage
}

type OnNetworkReport struct {
	AcceptURI string

	AcceptCID string

	Address string

	CreatedAt string
}

type Plane interface {
	LoginStep(ctx context.Context, step string, input map[string]any) Reply

	OIDCToken(ctx context.Context, role, vaultToken string) Reply

	Discovery(ctx context.Context) Reply

	Keys(ctx context.Context) Reply

	OnNetwork(ctx context.Context, authorization string, report OnNetworkReport) Reply
}

type Relay interface {
	URL() string

	Close()
}

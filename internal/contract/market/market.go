package market

import (
	"context"
	"encoding/json"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	RoleBidder = "bidder"

	RoleRequester = "requester"
)

const (
	OfferKindOffering = "offering"

	OfferKindRFP = "rfp"

	OfferKindBid = "bid"
)

const (
	StateAccepted = "accepted"

	StateActive = "active"

	StateSettled = "settled"
)

type Identity struct {
	DID string

	Handle string

	Role string
}

type Peer struct {
	DID string

	Role string

	Offering commonv1.StrongRef

	SeenAt time.Time
}

type Offer struct {
	Kind string

	Ref commonv1.StrongRef

	Author string

	Subject *commonv1.StrongRef

	Free bool

	Cost float64

	Value json.RawMessage
}

type OfferQuery struct {
	Kind string

	Author string

	Subject *commonv1.StrongRef

	Limit int
}

type Contract struct {
	RFP commonv1.StrongRef

	Bid commonv1.StrongRef

	Accept commonv1.StrongRef

	Receipt *commonv1.StrongRef

	Requester string

	Provider string

	State string
}

type ContractEvent struct {
	Ref commonv1.StrongRef

	Kind string

	Value json.RawMessage

	Time time.Time
}

type Participant interface {
	Identity(ctx context.Context) (Identity, error)

	Peers(ctx context.Context) ([]Peer, error)
}

type Offers interface {
	Publish(ctx context.Context, offer Offer) (commonv1.StrongRef, error)

	Solicit(ctx context.Context, peer string, rfp commonv1.StrongRef) ([]Offer, error)

	List(ctx context.Context, query OfferQuery) ([]Offer, error)

	Withdraw(ctx context.Context, ref commonv1.StrongRef) error
}

type Contracts interface {
	Accept(ctx context.Context, bid commonv1.StrongRef) (Contract, error)

	Contract(ctx context.Context, accept commonv1.StrongRef) (Contract, error)

	Report(ctx context.Context, contract commonv1.StrongRef, event ContractEvent) (commonv1.StrongRef, error)

	Events(ctx context.Context, contract commonv1.StrongRef) ([]ContractEvent, error)

	Settle(ctx context.Context, contract commonv1.StrongRef, reason string) error
}

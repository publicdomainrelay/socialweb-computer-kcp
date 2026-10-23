package market

import (
	"context"
	"errors"
	"time"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const CapabilityIntents = "market.intents"

const (
	KindBid = "Bid"

	KindComputeVM = "ComputeVM"

	KindGuestIdentityGrant = "GuestIdentityGrant"

	KindStateSync = "StateSync"

	KindComputeContract = "ComputeContract"

	KindExposure = "Exposure"

	KindMarketIdentity = "MarketIdentity"
)

var (
	ErrExists = errors.New("market: the object already exists")

	ErrNotFound = errors.New("market: the object does not exist")
)

type Change struct {
	Workspace string

	Kind string

	Name string

	Gone bool

	Bid *v1alpha1.Bid

	ComputeVM *v1alpha1.ComputeVM

	StateSync *v1alpha1.StateSync

	ComputeContract *v1alpha1.ComputeContract

	Exposure *exposurev1alpha1.Exposure

	MarketIdentity *v1alpha1.MarketIdentity
}

type Observer interface {
	Observe(ctx context.Context, change Change) (time.Duration, error)
}

type Subscription struct {
	Kinds []string

	Workspaces []string

	Observer Observer

	Synced func()
}

type Inputs interface {
	Subscribe(ctx context.Context, sub Subscription) (func(), error)
}

type BidIntents interface {
	CreateBid(ctx context.Context, bid v1alpha1.Bid) error

	ApplyBidSpec(ctx context.Context, name string, spec v1alpha1.BidSpec) error

	ApplyBidStatus(ctx context.Context, name string, status v1alpha1.BidStatus) error
}

type GuestIntents interface {
	CreateComputeVM(ctx context.Context, vm v1alpha1.ComputeVM) error

	DeleteComputeVM(ctx context.Context, name string) error

	ApplyComputeVMContract(ctx context.Context, name string, contract v1alpha1.ComputeVMContract) error

	ApplyComputeVMRelease(ctx context.Context, name string, release v1alpha1.ComputeVMRelease) error

	ApplyComputeVMPublished(ctx context.Context, name string, published v1alpha1.ComputeVMPublished) error

	CreateGuestIdentityGrant(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error
}

type StateSyncIntents interface {
	CreateStateSync(ctx context.Context, sync v1alpha1.StateSync) error

	DeleteStateSync(ctx context.Context, name string) error

	ApplyStateSyncStatus(ctx context.Context, name string, status v1alpha1.StateSyncStatus) error

	SetStateSyncFinalizer(ctx context.Context, name, finalizer string, present bool) error
}

type ContractIntents interface {
	CreateComputeContract(ctx context.Context, contract v1alpha1.ComputeContract) error

	ReleaseComputeContract(ctx context.Context, name string) error

	DeleteComputeContract(ctx context.Context, name string) error

	MergeComputeContractStatus(ctx context.Context, name string, status v1alpha1.ComputeContractStatus) error

	SetComputeContractPendingEvents(ctx context.Context, name string, events []v1alpha1.ComputeContractEvent) error
}

type Intents interface {
	Inputs

	BidIntents

	GuestIntents

	StateSyncIntents

	ContractIntents
}

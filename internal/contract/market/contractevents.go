package market

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const CapabilityContractEvents = "market.contractEvents"

const KindContractEvent = "ContractEvent"

type ContractEventChange struct {
	Workspace string

	Name string

	Gone bool

	Event *v1alpha1.ContractEvent

	Contract *v1alpha1.ComputeContract
}

type ContractEventObserver interface {
	ObserveContractEvent(ctx context.Context, change ContractEventChange) (time.Duration, error)
}

type ContractEvents interface {
	SubscribeContractEvents(ctx context.Context, observer ContractEventObserver) (func(), error)

	CreateContractEvent(ctx context.Context, event v1alpha1.ContractEvent) error

	DeleteContractEvent(ctx context.Context, name string) error

	ApplyContractEventOwner(ctx context.Context, name string, contract v1alpha1.ComputeContract) error

	ApplyContractEventStatus(ctx context.Context, name string, status v1alpha1.ContractEventStatus) error
}

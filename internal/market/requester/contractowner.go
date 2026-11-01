package requester

import (
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const foreignOwnerRetry = 2 * time.Second

func (q *Requester) contractOwner() string {
	return strings.TrimSpace(q.cfg.Spec.ContractOwnerDid)
}

func (q *Requester) ownsContract(contract v1alpha1.ComputeContract) bool {
	owner := q.contractOwner()
	return owner == "" || strings.TrimSpace(contract.Spec.OwnerDid) == owner
}

func (q *Requester) syncOwnership(item v1alpha1.StateSync) (mine bool, retry time.Duration) {
	if q.contractOwner() == "" {
		return true, 0
	}
	for _, ref := range item.OwnerReferences {
		if ref.Kind != "ComputeContract" {
			continue
		}
		if _, foreign := q.foreignContracts.Load(ref.Name); foreign {
			return false, 0
		}
		if _, ok := q.contracts.Get(ref.Name); !ok {
			return false, foreignOwnerRetry
		}
	}
	return true, 0
}

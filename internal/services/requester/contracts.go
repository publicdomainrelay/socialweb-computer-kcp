package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/statuspatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

var ComputeContractGVR = schema.GroupVersionResource{
	Group:    v1alpha1.GroupName,
	Version:  v1alpha1.Version,
	Resource: "computecontracts",
}

type ContractStore interface {
	List(ctx context.Context) ([]v1alpha1.ComputeContract, error)

	Get(ctx context.Context, name string) (*v1alpha1.ComputeContract, error)

	Create(ctx context.Context, contract *v1alpha1.ComputeContract) error

	Release(ctx context.Context, name string) error

	Delete(ctx context.Context, name string) error

	PatchStatus(ctx context.Context, name string, status v1alpha1.ComputeContractStatus) error

	AppendPendingEvent(ctx context.Context, name string, event v1alpha1.ComputeContractEvent) error

	SetPendingEvents(ctx context.Context, name string, events []v1alpha1.ComputeContractEvent) error
}

type KCPContracts struct {
	Res *kcpstore.Resource[v1alpha1.ComputeContract]

	Cluster string

	Namespace string
}

func NewKCPContracts(res *kcpstore.Resource[v1alpha1.ComputeContract], cluster, namespace string) *KCPContracts {
	return &KCPContracts{Res: res, Cluster: cluster, Namespace: namespace}
}

func (c *KCPContracts) List(ctx context.Context) ([]v1alpha1.ComputeContract, error) {
	if c == nil || c.Res == nil {
		return nil, errors.New("requester: the contract store is not configured")
	}
	if c.Namespace != "" {
		return c.Res.ListIn(ctx, c.Cluster, c.Namespace)
	}
	return c.Res.List(ctx, c.Cluster)
}

func (c *KCPContracts) Get(ctx context.Context, name string) (*v1alpha1.ComputeContract, error) {
	if c == nil || c.Res == nil {
		return nil, errors.New("requester: the contract store is not configured")
	}
	contract, err := c.Res.Get(ctx, ref.New(c.Cluster, "", name))
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return contract, err
}

func (c *KCPContracts) Create(ctx context.Context, contract *v1alpha1.ComputeContract) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the contract store is not configured")
	}
	return c.Res.Create(ctx, c.Cluster, contract)
}

func (c *KCPContracts) Release(ctx context.Context, name string) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the contract store is not configured")
	}
	return c.Res.PatchMerge(ctx, ref.New(c.Cluster, "", name), []byte(`{"spec":{"release":true}}`))
}

func (c *KCPContracts) Delete(ctx context.Context, name string) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the contract store is not configured")
	}
	err := c.Res.Delete(ctx, ref.New(c.Cluster, "", name))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *KCPContracts) PatchStatus(ctx context.Context, name string, status v1alpha1.ComputeContractStatus) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the contract store is not configured")
	}
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	patch, err := statuspatch.Merge(value)
	if err != nil {
		return err
	}
	return c.Res.PatchStatus(ctx, ref.New(c.Cluster, "", name), patch)
}

func (c *KCPContracts) AppendPendingEvent(ctx context.Context, name string, event v1alpha1.ComputeContractEvent) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the contract store is not configured")
	}
	for range 5 {
		current, err := c.Get(ctx, name)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("requester: the ComputeContract %s does not exist", name)
		}
		existing := current.Status.PendingEvents
		found := false
		for _, pending := range existing {
			if pending.URI == event.URI && (pending.CID == "" || event.CID == "" || pending.CID == event.CID) {
				found = true
				break
			}
		}
		if found {
			return nil
		}
		next := make([]v1alpha1.ComputeContractEvent, 0, len(existing)+1)
		next = append(next, existing...)
		next = append(next, event)
		patch, err := statuspatch.Merge(map[string]any{"pendingEvents": next})
		if err != nil {
			return err
		}
		target := ref.New(c.Cluster, "", name)
		target.ResourceVersion = current.ResourceVersion
		err = c.Res.PatchStatus(ctx, target, patch)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("requester: the pending event list of %s stayed in conflict", name)
}

func (c *KCPContracts) SetPendingEvents(ctx context.Context, name string, events []v1alpha1.ComputeContractEvent) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the contract store is not configured")
	}
	if events == nil {
		events = []v1alpha1.ComputeContractEvent{}
	}
	patch, err := statuspatch.Merge(map[string]any{"pendingEvents": events})
	if err != nil {
		return err
	}
	return c.Res.PatchStatus(ctx, ref.New(c.Cluster, "", name), patch)
}

func kcpStore(deps component.Deps, workspace string) (*kcpstore.Store, error) {
	if deps.KCP == nil {
		return nil, errors.New("requester: the component has no kcp clients")
	}
	if workspace == "" {
		workspace = "root"
	}
	client, err := deps.KCP.Client(workspace)
	if err != nil {
		return nil, fmt.Errorf("requester: kcp client for %s: %w", workspace, err)
	}
	cfg := client.RESTConfig()
	if cfg == nil || cfg.Host == "" {
		return nil, fmt.Errorf("requester: the kcp client for %s has no rest config", workspace)
	}
	return kcpstore.New(kcpstore.Options{Host: cfg.Host, RestConfig: cfg})
}

func contractPhaseDone(phase v1alpha1.ComputeContractPhase) bool {
	return phase == v1alpha1.ComputeContractReleased || phase == v1alpha1.ComputeContractFailed
}

func addContractConditions(status *v1alpha1.ComputeContractStatus, generation int64, phase v1alpha1.ComputeContractPhase, message string, now time.Time, atCapacity bool) {
	if atCapacity {
		setCondition(status, generation, "AtCapacity", metav1.ConditionTrue, "AdmissionFull", message, now)
	} else {
		removeCondition(status, "AtCapacity")
	}
	progressing := phase == v1alpha1.ComputeContractPending || phase == v1alpha1.ComputeContractRequesting || phase == v1alpha1.ComputeContractReleasing
	if progressing {
		setCondition(status, generation, v1alpha1.ComputeContractConditionProgressing, metav1.ConditionTrue, "Working", message, now)
	} else {
		setCondition(status, generation, v1alpha1.ComputeContractConditionProgressing, metav1.ConditionFalse, "Settled", message, now)
	}
	if phase == v1alpha1.ComputeContractHeld {
		setCondition(status, generation, v1alpha1.ComputeContractConditionHeld, metav1.ConditionTrue, "Held", message, now)
	} else {
		setCondition(status, generation, v1alpha1.ComputeContractConditionHeld, metav1.ConditionFalse, "NotHeld", message, now)
	}
	switch phase {
	case v1alpha1.ComputeContractReleased:
		setCondition(status, generation, "Complete", metav1.ConditionTrue, "Released", message, now)
	case v1alpha1.ComputeContractFailed:
		setCondition(status, generation, "Failed", metav1.ConditionTrue, "Failed", message, now)
	}
}

func setCondition(status *v1alpha1.ComputeContractStatus, generation int64, kind string, value metav1.ConditionStatus, reason, message string, now time.Time) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               kind,
		Status:             value,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             reason,
		Message:            message,
	})
}

func removeCondition(status *v1alpha1.ComputeContractStatus, kind string) {
	meta.RemoveStatusCondition(&status.Conditions, kind)
}

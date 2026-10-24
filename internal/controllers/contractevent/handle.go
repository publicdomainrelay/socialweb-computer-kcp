package contractevent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

type handle struct {
	c *Controller

	owner commonv1.ObjectRef

	manager string
}

var _ market.ContractEvents = (*handle)(nil)

func (c *Controller) open(_ context.Context, to commonv1.ObjectRef, _ []string) (market.ContractEvents, error) {
	if strings.TrimSpace(to.Workspace) == "" || strings.TrimSpace(to.Name) == "" || strings.TrimSpace(to.Kind) == "" {
		return nil, fmt.Errorf("contractevent: %s names no component", to.String())
	}
	return &handle{c: c, owner: to, manager: ctlruntime.FieldManagerPrefix + strings.ToLower(strings.TrimSpace(to.Kind))}, nil
}

func (h *handle) SubscribeContractEvents(ctx context.Context, observer market.ContractEventObserver) (func(), error) {
	return h.c.subscribe(ctx, h.owner.Workspace, observer)
}

func (h *handle) client() (client.Client, error) {
	return h.c.clients(h.owner.Workspace)
}

func skeleton(name string) kube.Doc {
	return kube.Doc{"apiVersion": v1alpha1.GroupVersion.String(), "kind": market.KindContractEvent, "metadata": map[string]any{"name": name}}
}

func encode(what string, value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("contractevent: encoding the %s: %w", what, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out := map[string]any{}
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("contractevent: encoding the %s: %w", what, err)
	}
	return out, nil
}

func (h *handle) apply(ctx context.Context, doc kube.Doc, status bool) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	return kube.ApplyWith(ctx, cl, doc, kube.ApplyOptions{FieldManager: h.manager, Force: true, Status: status})
}

func (h *handle) CreateContractEvent(ctx context.Context, event v1alpha1.ContractEvent) error {
	name := strings.TrimSpace(event.Name)
	if name == "" {
		return fmt.Errorf("contractevent: the ContractEvent names no object")
	}
	cl, err := h.client()
	if err != nil {
		return err
	}
	switch _, err := kube.GetWith(ctx, cl, v1alpha1.GroupVersion.WithKind(market.KindContractEvent), types.NamespacedName{Name: name}); {
	case err == nil:
		return market.ErrExists
	case !apierrors.IsNotFound(err):
		return err
	}
	doc := skeleton(name)
	meta := doc["metadata"].(map[string]any)
	if len(event.Labels) > 0 {
		labels := map[string]any{}
		for k, v := range event.Labels {
			labels[k] = v
		}
		meta["labels"] = labels
	}
	if len(event.OwnerReferences) > 0 {
		refs := make([]any, 0, len(event.OwnerReferences))
		for _, owner := range event.OwnerReferences {
			ref, err := encode("owner reference", owner)
			if err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		meta["ownerReferences"] = refs
	}
	spec, err := encode("ContractEvent spec", event.Spec)
	if err != nil {
		return err
	}
	doc["spec"] = spec
	if err := h.apply(ctx, doc, false); err != nil {
		return fmt.Errorf("contractevent: creating %s: %w", name, err)
	}
	return nil
}

func (h *handle) DeleteContractEvent(ctx context.Context, name string) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	if err := kube.DeleteWith(ctx, cl, v1alpha1.GroupVersion.WithKind(market.KindContractEvent), types.NamespacedName{Name: name},
		client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("contractevent: deleting %s: %w", name, err)
	}
	return nil
}

func ownerReference(contract v1alpha1.ComputeContract) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeContract", Name: contract.Name, UID: contract.UID}
}

func (h *handle) ApplyContractEventOwner(ctx context.Context, name string, contract v1alpha1.ComputeContract) error {
	if contract.Name == "" || contract.UID == "" {
		return fmt.Errorf("contractevent: the owner of %s names no ComputeContract", name)
	}
	ref, err := encode("owner reference", ownerReference(contract))
	if err != nil {
		return err
	}
	doc := skeleton(name)
	doc["metadata"].(map[string]any)["ownerReferences"] = []any{ref}
	if err := h.apply(ctx, doc, false); err != nil {
		return fmt.Errorf("contractevent: the owner of %s: %w", name, err)
	}
	return nil
}

func (h *handle) ApplyContractEventStatus(ctx context.Context, name string, status v1alpha1.ContractEventStatus) error {
	value, err := encode("ContractEvent status", status)
	if err != nil {
		return err
	}
	doc := skeleton(name)
	doc["status"] = value
	if err := h.apply(ctx, doc, true); err != nil {
		return fmt.Errorf("contractevent: applying the status of %s: %w", name, err)
	}
	return nil
}

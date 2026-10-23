package marketintents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	FieldManagerBidSpec = ctlruntime.FieldManagerPrefix + "bidder-spec"

	FieldManagerComputeVMContract = ctlruntime.FieldManagerPrefix + "bidder-contract"

	FieldManagerComputeVMRelease = ctlruntime.FieldManagerPrefix + "bidder-release"

	FieldManagerStateSyncStatus = ctlruntime.FieldManagerPrefix + "statesync"

	FieldManagerStateSyncFinalizer = ctlruntime.FieldManagerPrefix + "statesync/finalizer"

	FieldManagerContractRelease = ctlruntime.FieldManagerPrefix + "requester-release"

	ownerCacheTTL = time.Minute
)

type handle struct {
	c *Controller

	owner commonv1.ObjectRef

	manager string
}

var _ market.Intents = (*handle)(nil)

func (c *Controller) open(_ context.Context, to commonv1.ObjectRef, _ []string) (market.Intents, error) {
	if strings.TrimSpace(to.Workspace) == "" || strings.TrimSpace(to.Name) == "" || strings.TrimSpace(to.Kind) == "" {
		return nil, fmt.Errorf("marketintents: %s names no component", to.String())
	}
	return &handle{c: c, owner: to, manager: ctlruntime.FieldManagerPrefix + strings.ToLower(strings.TrimSpace(to.Kind))}, nil
}

func (h *handle) Subscribe(ctx context.Context, sub market.Subscription) (func(), error) {
	return h.c.subscribe(ctx, h.owner.Workspace, sub)
}

func (h *handle) client() (client.Client, error) {
	return h.c.clients(h.owner.Workspace)
}

func (h *handle) ownerRef(ctx context.Context) (metav1.OwnerReference, error) {
	cacheKey := h.owner.Workspace + "/" + h.owner.Kind + "/" + h.owner.Name
	h.c.mu.Lock()
	cached, ok := h.c.owners[cacheKey]
	h.c.mu.Unlock()
	ref := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: h.owner.Kind, Name: h.owner.Name}
	if ok && time.Since(cached.at) < ownerCacheTTL {
		ref.UID = types.UID(cached.uid)
		return ref, nil
	}
	cl, err := h.client()
	if err != nil {
		return ref, err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(h.owner.Kind))
	if err := cl.Get(ctx, types.NamespacedName{Name: h.owner.Name}, u); err != nil {
		return ref, fmt.Errorf("marketintents: reading the owner %s: %w", h.owner.String(), err)
	}
	ref.UID = u.GetUID()
	h.c.mu.Lock()
	h.c.owners[cacheKey] = ownerUID{uid: string(u.GetUID()), at: time.Now()}
	h.c.mu.Unlock()
	return ref, nil
}

func encode(kind string, value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marketintents: encoding the %s: %w", kind, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out := map[string]any{}
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("marketintents: encoding the %s: %w", kind, err)
	}
	return out, nil
}

func skeleton(kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}}}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	return u
}

func (h *handle) exists(ctx context.Context, kind, name string) (bool, error) {
	cl, err := h.client()
	if err != nil {
		return false, err
	}
	err = cl.Get(ctx, types.NamespacedName{Name: name}, skeleton(kind, name))
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

func (h *handle) apply(ctx context.Context, u *unstructured.Unstructured, manager string) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	if err := cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(manager), client.ForceOwnership); err != nil {
		return fmt.Errorf("marketintents: applying %s %s: %w", u.GetKind(), u.GetName(), err)
	}
	return nil
}

func (h *handle) applyStatus(ctx context.Context, kind, name string, status map[string]any, manager string) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	u := skeleton(kind, name)
	u.Object["status"] = status
	if err := cl.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(manager), client.ForceOwnership); err != nil {
		return fmt.Errorf("marketintents: applying the status of %s %s: %w", kind, name, err)
	}
	return nil
}

func (h *handle) create(ctx context.Context, kind string, meta metav1.ObjectMeta, spec any, manager string) error {
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		return fmt.Errorf("marketintents: the %s names no object", kind)
	}
	found, err := h.exists(ctx, kind, name)
	if err != nil {
		return err
	}
	if found {
		return market.ErrExists
	}
	owners := meta.OwnerReferences
	if len(owners) == 0 {
		owner, err := h.ownerRef(ctx)
		if err != nil {
			return err
		}
		owners = []metav1.OwnerReference{owner}
	}
	u := skeleton(kind, name)
	if len(meta.Labels) > 0 {
		u.SetLabels(meta.Labels)
	}
	if len(meta.Annotations) > 0 {
		u.SetAnnotations(meta.Annotations)
	}
	u.SetOwnerReferences(owners)
	value, err := encode(kind+" spec", spec)
	if err != nil {
		return err
	}
	u.Object["spec"] = value
	return h.apply(ctx, u, manager)
}

func (h *handle) applySpecField(ctx context.Context, kind, name, field string, value any, manager string) error {
	found, err := h.exists(ctx, kind, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s %s", market.ErrNotFound, kind, name)
	}
	encoded, err := encode(kind+" "+field, map[string]any{field: value})
	if err != nil {
		return err
	}
	u := skeleton(kind, name)
	u.Object["spec"] = encoded
	return h.apply(ctx, u, manager)
}

func (h *handle) remove(ctx context.Context, kind, name string) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	if err := cl.Delete(ctx, skeleton(kind, name), client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("marketintents: deleting %s %s: %w", kind, name, err)
	}
	return nil
}

func (h *handle) CreateBid(ctx context.Context, bid v1alpha1.Bid) error {
	return h.create(ctx, market.KindBid, bid.ObjectMeta, bid.Spec, v1alpha1.FieldManagerBid)
}

func (h *handle) ApplyBidSpec(ctx context.Context, name string, spec v1alpha1.BidSpec) error {
	found, err := h.exists(ctx, market.KindBid, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: Bid %s", market.ErrNotFound, name)
	}
	value, err := encode("Bid spec", spec)
	if err != nil {
		return err
	}
	u := skeleton(market.KindBid, name)
	u.Object["spec"] = value
	return h.apply(ctx, u, FieldManagerBidSpec)
}

func (h *handle) ApplyBidStatus(ctx context.Context, name string, status v1alpha1.BidStatus) error {
	value, err := encode("Bid status", status)
	if err != nil {
		return err
	}
	return h.applyStatus(ctx, market.KindBid, name, value, v1alpha1.FieldManagerBid)
}

func (h *handle) CreateComputeVM(ctx context.Context, vm v1alpha1.ComputeVM) error {
	return h.create(ctx, market.KindComputeVM, vm.ObjectMeta, vm.Spec, v1alpha1.FieldManagerComputeVMCreator)
}

func (h *handle) DeleteComputeVM(ctx context.Context, name string) error {
	return h.remove(ctx, market.KindComputeVM, name)
}

func (h *handle) ApplyComputeVMContract(ctx context.Context, name string, contract v1alpha1.ComputeVMContract) error {
	return h.applySpecField(ctx, market.KindComputeVM, name, "contract", contract, FieldManagerComputeVMContract)
}

func (h *handle) ApplyComputeVMRelease(ctx context.Context, name string, release v1alpha1.ComputeVMRelease) error {
	return h.applySpecField(ctx, market.KindComputeVM, name, "release", release, FieldManagerComputeVMRelease)
}

func (h *handle) ApplyComputeVMPublished(ctx context.Context, name string, published v1alpha1.ComputeVMPublished) error {
	value, err := encode("ComputeVM published", map[string]any{"market": map[string]any{"published": published}})
	if err != nil {
		return err
	}
	return h.applyStatus(ctx, market.KindComputeVM, name, value, v1alpha1.FieldManagerComputeVMMarketPublished)
}

func (h *handle) CreateGuestIdentityGrant(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error {
	return h.create(ctx, market.KindGuestIdentityGrant, grant.ObjectMeta, grant.Spec, h.manager)
}

func (h *handle) CreateStateSync(ctx context.Context, sync v1alpha1.StateSync) error {
	return h.create(ctx, market.KindStateSync, sync.ObjectMeta, sync.Spec, h.manager)
}

func (h *handle) DeleteStateSync(ctx context.Context, name string) error {
	return h.remove(ctx, market.KindStateSync, name)
}

func (h *handle) ApplyStateSyncStatus(ctx context.Context, name string, status v1alpha1.StateSyncStatus) error {
	value, err := encode("StateSync status", status)
	if err != nil {
		return err
	}
	err = h.applyStatus(ctx, market.KindStateSync, name, value, FieldManagerStateSyncStatus)
	if apierrors.IsNotFound(errors.Unwrap(err)) {
		return nil
	}
	return err
}

func (h *handle) SetStateSyncFinalizer(ctx context.Context, name, finalizer string, present bool) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	live := skeleton(market.KindStateSync, name)
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	has := false
	for _, f := range live.GetFinalizers() {
		if f == finalizer {
			has = true
		}
	}
	if has == present {
		return nil
	}
	u := skeleton(market.KindStateSync, name)
	if present {
		u.SetFinalizers([]string{finalizer})
	}
	return h.apply(ctx, u, FieldManagerStateSyncFinalizer)
}

func (h *handle) CreateComputeContract(ctx context.Context, contract v1alpha1.ComputeContract) error {
	return h.create(ctx, market.KindComputeContract, contract.ObjectMeta, contract.Spec, h.manager)
}

func (h *handle) ReleaseComputeContract(ctx context.Context, name string) error {
	return h.applySpecField(ctx, market.KindComputeContract, name, "release", true, FieldManagerContractRelease)
}

func (h *handle) DeleteComputeContract(ctx context.Context, name string) error {
	return h.remove(ctx, market.KindComputeContract, name)
}

func (h *handle) MergeComputeContractStatus(ctx context.Context, name string, status v1alpha1.ComputeContractStatus) error {
	value, err := encode("ComputeContract status", status)
	if err != nil {
		return err
	}
	return h.mergeStatus(ctx, market.KindComputeContract, name, value)
}

func (h *handle) SetComputeContractPendingEvents(ctx context.Context, name string, events []v1alpha1.ComputeContractEvent) error {
	if events == nil {
		events = []v1alpha1.ComputeContractEvent{}
	}
	value, err := encode("ComputeContract pending events", map[string]any{"pendingEvents": events})
	if err != nil {
		return err
	}
	return h.mergeStatus(ctx, market.KindComputeContract, name, value)
}

func (h *handle) mergeStatus(ctx context.Context, kind, name string, patch map[string]any) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	live := skeleton(kind, name)
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
		return fmt.Errorf("marketintents: reading %s %s: %w", kind, name, err)
	}
	current, _, _ := unstructured.NestedMap(live.Object, "status")
	merged := mergePatch(kruntime.DeepCopyJSON(current), patch)
	return h.applyStatus(ctx, kind, name, merged, h.manager)
}

func mergePatch(target, patch map[string]any) map[string]any {
	if target == nil {
		target = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		next, isMap := v.(map[string]any)
		if !isMap {
			target[k] = v
			continue
		}
		existing, _ := target[k].(map[string]any)
		target[k] = mergePatch(existing, next)
	}
	return target
}

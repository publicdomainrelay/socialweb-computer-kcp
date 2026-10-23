package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

const finalizerManagerSuffix = "/finalizer"

func (r Request) FieldManager() string {
	if r.scope == nil {
		return ""
	}
	return r.scope.fieldManager
}

func (r Request) Get(ctx context.Context, namespace, name string, obj Object) error {
	if r.scope == nil || r.scope.client == nil {
		return errors.New("runtime: the request has no cluster client")
	}
	return r.scope.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj)
}

func (r Request) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.scope == nil || r.scope.client == nil {
		return errors.New("runtime: the request has no cluster client")
	}
	return r.scope.client.List(ctx, list, opts...)
}

func (r Request) ApplyStatus(ctx context.Context, obj Object) error {
	if r.scope == nil || r.scope.client == nil {
		return errors.New("runtime: the request has no cluster client")
	}
	full, err := r.unstructured(obj)
	if err != nil {
		return err
	}
	u := r.skeleton(full, obj)
	status, _ := full.Object["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	if !r.scope.sharedStatus {
		if live, err := r.liveStatus(ctx, obj); err == nil {
			dropped(live, status)
		}
	}
	u.Object["status"] = status
	if err := r.scope.client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(r.scope.fieldManager), client.ForceOwnership); err != nil {
		if apierrors.IsMethodNotSupported(err) || apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("runtime: applying the status of %s: %w", obj.GetName(), err)
	}
	return nil
}

func (r Request) Apply(ctx context.Context, obj Object) error {
	if r.scope == nil || r.scope.client == nil {
		return errors.New("runtime: the request has no cluster client")
	}
	full, err := r.unstructured(obj)
	if err != nil {
		return err
	}
	u := r.skeleton(full, obj)
	for k, v := range full.Object {
		if k != "apiVersion" && k != "kind" && k != "metadata" && k != "status" {
			u.Object[k] = v
		}
	}
	meta, _ := u.Object["metadata"].(map[string]any)
	if labels := obj.GetLabels(); len(labels) > 0 {
		meta["labels"] = strs.AnyMap(labels)
	}
	if annotations := obj.GetAnnotations(); len(annotations) > 0 {
		meta["annotations"] = strs.AnyMap(annotations)
	}
	if owners, ok, _ := unstructured.NestedSlice(full.Object, "metadata", "ownerReferences"); ok && len(owners) > 0 {
		meta["ownerReferences"] = owners
	}
	if err := r.scope.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(r.scope.fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("runtime: applying %s: %w", obj.GetName(), err)
	}
	return nil
}

func (r Request) Delete(ctx context.Context, obj Object) error {
	if r.scope == nil || r.scope.client == nil {
		return errors.New("runtime: the request has no cluster client")
	}
	if err := r.scope.client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("runtime: deleting %s: %w", obj.GetName(), err)
	}
	return nil
}

func (r Request) applyFinalizer(ctx context.Context, obj Object, present bool) error {
	if !present {
		return r.releaseFinalizer(ctx, obj)
	}
	full, err := r.unstructured(obj)
	if err != nil {
		return err
	}
	u := r.skeleton(full, obj)
	meta, _ := u.Object["metadata"].(map[string]any)
	meta["finalizers"] = []any{r.scope.finalizer}
	if err := r.scope.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(r.scope.fieldManager+finalizerManagerSuffix), client.ForceOwnership); err != nil {
		return fmt.Errorf("runtime: applying the finalizer of %s: %w", obj.GetName(), err)
	}
	return nil
}

func (r Request) releaseFinalizer(ctx context.Context, obj Object) error {
	current := obj.GetFinalizers()
	remaining := slices.DeleteFunc(slices.Clone(current), func(f string) bool { return f == r.scope.finalizer })
	if len(remaining) == len(current) {
		return nil
	}
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/finalizers", "value": current},
		{"op": "replace", "path": "/metadata/finalizers", "value": remaining},
	})
	if err != nil {
		return fmt.Errorf("runtime: encoding the finalizer release of %s: %w", obj.GetName(), err)
	}
	if err := r.scope.client.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, body)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("runtime: releasing the finalizer of %s: %w", obj.GetName(), err)
	}
	return nil
}

func (r Request) unstructured(obj Object) (*unstructured.Unstructured, error) {
	gvk, err := apiutil.GVKForObject(obj, r.scope.scheme)
	if err != nil {
		return nil, fmt.Errorf("runtime: the kind of %T: %w", obj, err)
	}
	m, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("runtime: encoding %s: %w", obj.GetName(), err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(gvk)
	return u, nil
}

func (r Request) skeleton(full *unstructured.Unstructured, obj Object) *unstructured.Unstructured {
	meta := map[string]any{"name": obj.GetName()}
	if ns := obj.GetNamespace(); ns != "" {
		meta["namespace"] = ns
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": full.GetAPIVersion(),
		"kind":       full.GetKind(),
		"metadata":   meta,
	}}
}

func (r Request) liveStatus(ctx context.Context, obj Object) (map[string]any, error) {
	gvk, err := apiutil.GVKForObject(obj, r.scope.scheme)
	if err != nil {
		return nil, err
	}
	fresh, err := r.scope.scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	live, ok := fresh.(client.Object)
	if !ok {
		return nil, fmt.Errorf("runtime: %T is not an object", fresh)
	}
	if err := r.scope.client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, live); err != nil {
		return nil, err
	}
	m, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(live)
	if err != nil {
		return nil, err
	}
	status, _ := m["status"].(map[string]any)
	return status, nil
}

func dropped(live, desired map[string]any) {
	for k, v := range live {
		next, ok := desired[k]
		if !ok {
			desired[k] = nil
			continue
		}
		liveMap, liveIsMap := v.(map[string]any)
		nextMap, nextIsMap := next.(map[string]any)
		if liveIsMap && nextIsMap {
			dropped(liveMap, nextMap)
		}
	}
}

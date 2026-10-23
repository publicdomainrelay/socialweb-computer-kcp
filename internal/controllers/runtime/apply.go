package runtime

import (
	"context"
	"errors"
	"fmt"

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

func (r Request) ApplyStatus(ctx context.Context, obj Object) error {
	if r.scope == nil || r.scope.client == nil {
		return errors.New("runtime: the request has no cluster client")
	}
	full, err := r.unstructured(obj)
	if err != nil {
		return err
	}
	u := r.skeleton(full, obj)
	status, ok := full.Object["status"]
	if !ok || status == nil {
		status = map[string]any{}
	}
	u.Object["status"] = status
	if err := r.scope.client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(r.scope.fieldManager), client.ForceOwnership); err != nil {
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
		meta["labels"] = toAny(labels)
	}
	if annotations := obj.GetAnnotations(); len(annotations) > 0 {
		meta["annotations"] = toAny(annotations)
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

func (r Request) applyFinalizer(ctx context.Context, obj Object, present bool) error {
	full, err := r.unstructured(obj)
	if err != nil {
		return err
	}
	u := r.skeleton(full, obj)
	if present {
		meta, _ := u.Object["metadata"].(map[string]any)
		meta["finalizers"] = []any{r.scope.finalizer}
	}
	if err := r.scope.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(r.scope.fieldManager+finalizerManagerSuffix), client.ForceOwnership); err != nil {
		return fmt.Errorf("runtime: applying the finalizer of %s: %w", obj.GetName(), err)
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

func toAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

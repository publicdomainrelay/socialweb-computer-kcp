package kube

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func GetWith(ctx context.Context, cl client.Client, gvk schema.GroupVersionKind, key client.ObjectKey) (Doc, error) {
	if cl == nil {
		return nil, errors.New("kube: reading needs a client")
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := cl.Get(ctx, key, u); err != nil {
		return nil, err
	}
	return u.Object, nil
}

func DeleteWith(ctx context.Context, cl client.Client, gvk schema.GroupVersionKind, key client.ObjectKey, opts ...client.DeleteOption) error {
	if cl == nil {
		return errors.New("kube: deleting needs a client")
	}
	u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": key.Name}}}
	if key.Namespace != "" {
		u.SetNamespace(key.Namespace)
	}
	u.SetGroupVersionKind(gvk)
	return cl.Delete(ctx, u, opts...)
}

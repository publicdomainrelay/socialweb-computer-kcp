package kcpstore

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

type ClusterResource[T any] struct {
	store *Store

	gvr schema.GroupVersionResource
}

func NewClusterResource[T any](store *Store, gvr schema.GroupVersionResource) *ClusterResource[T] {
	return &ClusterResource[T]{store: store, gvr: gvr}
}

func (r *ClusterResource[T]) client(cluster string) (rest.Interface, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("kcpstore: the kcp store is not configured")
	}
	return r.store.ClientFor(cluster, r.gvr.GroupVersion())
}

func (r *ClusterResource[T]) Get(ctx context.Context, cluster, name string) (*T, error) {
	c, err := r.client(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := c.Get().Resource(r.gvr.Resource).Name(name).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("kcpstore: read %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("kcpstore: decode %s: %w", r.gvr.Resource, err)
	}
	return &out, nil
}

func (r *ClusterResource[T]) List(ctx context.Context, cluster string) ([]T, error) {
	c, err := r.client(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := c.Get().Resource(r.gvr.Resource).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("kcpstore: list %s in %s: %w", r.gvr.Resource, cluster, err)
	}
	var list struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("kcpstore: decode %s list: %w", r.gvr.Resource, err)
	}
	return list.Items, nil
}

func (r *ClusterResource[T]) Create(ctx context.Context, cluster string, obj *T) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("kcpstore: encode %s: %w", r.gvr.Resource, err)
	}
	response, err := c.Post().Resource(r.gvr.Resource).Body(raw).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("kcpstore: create %s %s in %s: %w%s", r.gvr.Resource, NameOf(raw), cluster, err, ResponseDetail(response))
	}
	return nil
}

func (r *ClusterResource[T]) Delete(ctx context.Context, cluster, name string) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	err = c.Delete().Resource(r.gvr.Resource).Name(name).Body([]byte(`{"propagationPolicy":"Background"}`)).Do(ctx).Error()
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kcpstore: delete %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	return nil
}

func (r *ClusterResource[T]) ApplyStatus(ctx context.Context, cluster, kind, name, fieldManager string, status map[string]any) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": r.gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"status":     status,
	})
	if err != nil {
		return fmt.Errorf("kcpstore: encode %s status: %w", r.gvr.Resource, err)
	}
	response, err := c.Patch(types.ApplyPatchType).SubResource("status").Resource(r.gvr.Resource).Name(name).
		Param("fieldManager", fieldManager).Param("force", "true").Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("kcpstore: apply %s status for %s in %s: %w%s", r.gvr.Resource, name, cluster, err, ResponseDetail(response))
	}
	return nil
}

func (r *ClusterResource[T]) ApplySpec(ctx context.Context, cluster, kind, name, fieldManager string, spec map[string]any) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": r.gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	})
	if err != nil {
		return fmt.Errorf("kcpstore: encode %s spec: %w", r.gvr.Resource, err)
	}
	response, err := c.Patch(types.ApplyPatchType).Resource(r.gvr.Resource).Name(name).
		Param("fieldManager", fieldManager).Param("force", "true").Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("kcpstore: apply %s spec for %s in %s: %w%s", r.gvr.Resource, name, cluster, err, ResponseDetail(response))
	}
	return nil
}

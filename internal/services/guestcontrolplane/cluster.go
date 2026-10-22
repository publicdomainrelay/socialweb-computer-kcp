package guestcontrolplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/statuspatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

type clusterResource[T any] struct {
	store *kcpstore.Store

	gvr schema.GroupVersionResource
}

func newClusterResource[T any](store *kcpstore.Store, gvr schema.GroupVersionResource) *clusterResource[T] {
	return &clusterResource[T]{store: store, gvr: gvr}
}

func (r *clusterResource[T]) client(cluster string) (rest.Interface, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("guestcontrolplane: the kcp store is not configured")
	}
	return r.store.ClientFor(cluster, r.gvr.GroupVersion())
}

func (r *clusterResource[T]) Get(ctx context.Context, cluster, name string) (*T, error) {
	c, err := r.client(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := c.Get().Resource(r.gvr.Resource).Name(name).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("guestcontrolplane: read %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("guestcontrolplane: decode %s: %w", r.gvr.Resource, err)
	}
	return &out, nil
}

func (r *clusterResource[T]) List(ctx context.Context, cluster string) ([]T, error) {
	c, err := r.client(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := c.Get().Resource(r.gvr.Resource).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("guestcontrolplane: list %s in %s: %w", r.gvr.Resource, cluster, err)
	}
	var list struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("guestcontrolplane: decode %s list: %w", r.gvr.Resource, err)
	}
	return list.Items, nil
}

func (r *clusterResource[T]) Create(ctx context.Context, cluster string, obj *T) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("guestcontrolplane: encode %s: %w", r.gvr.Resource, err)
	}
	response, err := c.Post().Resource(r.gvr.Resource).Body(raw).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("guestcontrolplane: create %s %s in %s: %w%s", r.gvr.Resource, nameOf(raw), cluster, err, detail(response))
	}
	return nil
}

func (r *clusterResource[T]) Delete(ctx context.Context, cluster, name string) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	err = c.Delete().Resource(r.gvr.Resource).Name(name).Body([]byte(`{"propagationPolicy":"Background"}`)).Do(ctx).Error()
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("guestcontrolplane: delete %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	return nil
}

func (r *clusterResource[T]) PatchStatus(ctx context.Context, cluster, name, resourceVersion string, patch []byte) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	body, err := statuspatch.WithResourceVersion(patch, resourceVersion)
	if err != nil {
		return err
	}
	response, err := c.Patch(types.MergePatchType).SubResource("status").Resource(r.gvr.Resource).Name(name).Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("guestcontrolplane: write %s status for %s in %s: %w%s", r.gvr.Resource, name, cluster, err, detail(response))
	}
	return nil
}

func (r *clusterResource[T]) ApplyStatus(ctx context.Context, cluster, kind, name, fieldManager string, status map[string]any) error {
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
		return fmt.Errorf("guestcontrolplane: encode %s status: %w", r.gvr.Resource, err)
	}
	response, err := c.Patch(types.ApplyPatchType).SubResource("status").Resource(r.gvr.Resource).Name(name).
		Param("fieldManager", fieldManager).Param("force", "true").Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("guestcontrolplane: apply %s status for %s in %s: %w%s", r.gvr.Resource, name, cluster, err, detail(response))
	}
	return nil
}

func (r *clusterResource[T]) PatchJSON(ctx context.Context, cluster, name string, patch []byte) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	response, err := c.Patch(types.JSONPatchType).Resource(r.gvr.Resource).Name(name).Body(patch).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("guestcontrolplane: patch %s %s in %s: %w%s", r.gvr.Resource, name, cluster, err, detail(response))
	}
	return nil
}

func detail(response []byte) string {
	if len(response) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(string(response))
}

func nameOf(raw []byte) string {
	var obj struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.Metadata.Name
}

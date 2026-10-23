package bidder

import (
	"context"
	"encoding/json"
	"fmt"

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
		return nil, fmt.Errorf("bidder: the kcp store is not configured")
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
		return nil, fmt.Errorf("bidder: read %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bidder: decode %s: %w", r.gvr.Resource, err)
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
		return nil, fmt.Errorf("bidder: list %s in %s: %w", r.gvr.Resource, cluster, err)
	}
	var list struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("bidder: decode %s list: %w", r.gvr.Resource, err)
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
		return fmt.Errorf("bidder: encode %s: %w", r.gvr.Resource, err)
	}
	response, err := c.Post().Resource(r.gvr.Resource).Body(raw).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("bidder: create %s %s in %s: %w%s", r.gvr.Resource, kcpstore.NameOf(raw), cluster, err, kcpstore.ResponseDetail(response))
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
		return fmt.Errorf("bidder: delete %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
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
		return fmt.Errorf("bidder: write %s status for %s in %s: %w%s", r.gvr.Resource, name, cluster, err, kcpstore.ResponseDetail(response))
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
		return fmt.Errorf("bidder: patch %s %s in %s: %w%s", r.gvr.Resource, name, cluster, err, kcpstore.ResponseDetail(response))
	}
	return nil
}

func (r *clusterResource[T]) PatchMerge(ctx context.Context, cluster, name string, patch []byte) error {
	c, err := r.client(cluster)
	if err != nil {
		return err
	}
	response, err := c.Patch(types.MergePatchType).Resource(r.gvr.Resource).Name(name).Body(patch).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("bidder: patch %s %s in %s: %w%s", r.gvr.Resource, name, cluster, err, kcpstore.ResponseDetail(response))
	}
	return nil
}

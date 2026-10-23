package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcprest"
)

type Doc = map[string]any

type Client struct {
	dyn dynamic.Interface
}

func New(cfg *rest.Config) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("kube: a rest config is required")
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: the client for %s: %w", cfg.Host, err)
	}
	return &Client{dyn: dyn}, nil
}

func ForWorkspace(base *rest.Config, workspace string) (*Client, error) {
	if base == nil {
		return nil, errors.New("kube: a rest config is required")
	}
	return New(kcprest.ForCluster(base, workspace))
}

type Factory func(workspace string) (*Client, error)

func NewFactory(base *rest.Config) Factory {
	return func(workspace string) (*Client, error) { return ForWorkspace(base, workspace) }
}

type Resource struct {
	ri  dynamic.NamespaceableResourceInterface
	gvr schema.GroupVersionResource
	ns  string
}

func (c *Client) Resource(gvr schema.GroupVersionResource) Resource {
	if c == nil || c.dyn == nil {
		return Resource{gvr: gvr}
	}
	return Resource{ri: c.dyn.Resource(gvr), gvr: gvr}
}

func (r Resource) In(namespace string) Resource {
	r.ns = namespace
	return r
}

func (r Resource) GVR() schema.GroupVersionResource { return r.gvr }

func (r Resource) Namespace() string { return r.ns }

func (r Resource) iface() (dynamic.ResourceInterface, error) {
	if r.ri == nil {
		return nil, fmt.Errorf("kube: %s has no client", r.gvr.Resource)
	}
	if r.ns != "" {
		return r.ri.Namespace(r.ns), nil
	}
	return r.ri, nil
}

type ApplyOptions struct {
	FieldManager string

	Force bool

	Status bool

	DryRun []string
}

func Get[T any](ctx context.Context, r Resource, name string) (*T, error) {
	ri, err := r.iface()
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return Decode[T](u.Object)
}

func List[T any](ctx context.Context, r Resource, opts metav1.ListOptions) ([]T, error) {
	ri, err := r.iface()
	if err != nil {
		return nil, err
	}
	l, err := ri.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(l.Items))
	for i := range l.Items {
		v, err := Decode[T](l.Items[i].Object)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func Create[T any](ctx context.Context, r Resource, obj any, opts metav1.CreateOptions) (*T, error) {
	ri, u, err := r.prepare(obj)
	if err != nil {
		return nil, err
	}
	got, err := ri.Create(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	return Decode[T](got.Object)
}

func Update[T any](ctx context.Context, r Resource, obj any, opts metav1.UpdateOptions) (*T, error) {
	ri, u, err := r.prepare(obj)
	if err != nil {
		return nil, err
	}
	got, err := ri.Update(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	return Decode[T](got.Object)
}

func UpdateStatus[T any](ctx context.Context, r Resource, obj any, opts metav1.UpdateOptions) (*T, error) {
	ri, u, err := r.prepare(obj)
	if err != nil {
		return nil, err
	}
	got, err := ri.UpdateStatus(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	return Decode[T](got.Object)
}

func Apply[T any](ctx context.Context, r Resource, obj any, o ApplyOptions) (*T, error) {
	ri, err := r.iface()
	if err != nil {
		return nil, err
	}
	doc, err := ApplyBody(obj, o.Status)
	if err != nil {
		return nil, err
	}
	name, _, _ := unstructured.NestedString(doc, "metadata", "name")
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("kube: applying %s: the object has no name", r.gvr.Resource)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("kube: encoding %s %s: %w", r.gvr.Resource, name, err)
	}
	opts := metav1.PatchOptions{FieldManager: o.FieldManager, DryRun: o.DryRun}
	if o.Force {
		opts.Force = new(true)
	}
	var sub []string
	if o.Status {
		sub = []string{"status"}
	}
	got, err := ri.Patch(ctx, name, types.ApplyPatchType, body, opts, sub...)
	if err != nil {
		return nil, err
	}
	return Decode[T](got.Object)
}

func Patch[T any](ctx context.Context, r Resource, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*T, error) {
	ri, err := r.iface()
	if err != nil {
		return nil, err
	}
	got, err := ri.Patch(ctx, name, pt, data, opts, subresources...)
	if err != nil {
		return nil, err
	}
	return Decode[T](got.Object)
}

func (r Resource) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	ri, err := r.iface()
	if err != nil {
		return err
	}
	return ri.Delete(ctx, name, opts)
}

func (r Resource) prepare(obj any) (dynamic.ResourceInterface, *unstructured.Unstructured, error) {
	ri, err := r.iface()
	if err != nil {
		return nil, nil, err
	}
	doc, err := Encode(obj)
	if err != nil {
		return nil, nil, err
	}
	return ri, &unstructured.Unstructured{Object: doc}, nil
}

func ApplyBody(obj any, status bool) (Doc, error) {
	if d, ok := asDoc(obj); ok {
		av, _ := d["apiVersion"].(string)
		kind, _ := d["kind"].(string)
		if av == "" || kind == "" {
			return nil, errors.New("kube: an applied document needs apiVersion and kind")
		}
		return d, nil
	}
	full, err := Encode(obj)
	if err != nil {
		return nil, err
	}
	meta, _ := full["metadata"].(map[string]any)
	out := Doc{"apiVersion": full["apiVersion"], "kind": full["kind"]}
	skeleton := map[string]any{}
	keys := []string{"name", "namespace"}
	if !status {
		keys = append(keys, "labels", "annotations", "ownerReferences", "finalizers")
	}
	for _, k := range keys {
		if v, ok := meta[k]; ok && v != nil {
			skeleton[k] = v
		}
	}
	out["metadata"] = skeleton
	if status {
		st, _ := full["status"].(map[string]any)
		if st == nil {
			st = map[string]any{}
		}
		out["status"] = st
		return out, nil
	}
	for k, v := range full {
		if k != "apiVersion" && k != "kind" && k != "metadata" && k != "status" {
			out[k] = v
		}
	}
	return out, nil
}

func ApplyWith(ctx context.Context, cl client.Client, obj any, o ApplyOptions) error {
	if cl == nil {
		return errors.New("kube: applying needs a client")
	}
	doc, err := ApplyBody(obj, o.Status)
	if err != nil {
		return err
	}
	ac := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: doc})
	if o.Status {
		opts := []client.SubResourceApplyOption{client.FieldOwner(o.FieldManager)}
		if o.Force {
			opts = append(opts, client.ForceOwnership)
		}
		return cl.Status().Apply(ctx, ac, opts...)
	}
	opts := []client.ApplyOption{client.FieldOwner(o.FieldManager)}
	if o.Force {
		opts = append(opts, client.ForceOwnership)
	}
	if len(o.DryRun) > 0 {
		opts = append(opts, client.DryRunAll)
	}
	return cl.Apply(ctx, ac, opts...)
}

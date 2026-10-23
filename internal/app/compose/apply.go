package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ReasonChildEditReverted = "ChildEditReverted"

	eventNamespace = "default"
)

type Desired struct {
	Kind string

	Name string

	Spec any
}

func DesiredKeys(desired []Desired) []string {
	out := make([]string, 0, len(desired))
	for _, d := range desired {
		out = append(out, ComponentKey(d.Kind, d.Name))
	}
	return out
}

func SpecMap(spec any) (map[string]any, error) {
	if spec == nil {
		return nil, nil
	}
	if m, ok := spec.(map[string]any); ok {
		return m, nil
	}
	v := reflect.ValueOf(spec)
	if v.Kind() != reflect.Pointer {
		ptr := reflect.New(v.Type())
		ptr.Elem().Set(v)
		spec = ptr.Interface()
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(spec)
	if err != nil {
		return nil, failf("the component spec: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

func (c *Composer) Object(r Root, ws, apiVersion, kind, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	u.SetName(name)
	merged := r.Labels("")
	for k, v := range labels {
		merged[k] = v
	}
	u.SetLabels(merged)
	if ws == "" || ws == r.Workspace {
		u.SetOwnerReferences([]metav1.OwnerReference{r.OwnerRef()})
	}
	return u
}

func (c *Composer) ApplyComponent(ctx context.Context, r Root, ws string, d Desired) error {
	ct, err := componentTypeFor(d.Kind)
	if err != nil {
		return err
	}
	spec, err := SpecMap(d.Spec)
	if err != nil {
		return err
	}
	ws = workspaceOr(ws, r.Workspace)
	u := c.Object(r, ws, ct.APIVersion(), ct.Kind, d.Name, map[string]string{ComponentLabel: d.Kind})
	if spec != nil {
		u.Object["spec"] = spec
	}
	return c.Apply(ctx, r, ws, ct.GVR, u)
}

func (c *Composer) EnsureComponents(ctx context.Context, r Root, desired []Desired) error {
	for _, d := range desired {
		if err := c.ApplyComponent(ctx, r, r.Workspace, d); err != nil {
			return fmt.Errorf("%s %s: %w", d.Kind, d.Name, err)
		}
	}
	return nil
}

func workspaceOr(ws, fallback string) string {
	if ws = strings.TrimSpace(ws); ws != "" {
		return ws
	}
	return fallback
}

func (c *Composer) Apply(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) error {
	return c.applyAs(ctx, r, r.Manager(), ws, gvr, u)
}

func (c *Composer) applyAs(ctx context.Context, r Root, manager, ws string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) error {
	ws = workspaceOr(ws, r.Workspace)
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return err
	}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	var rc = ri.Namespace(u.GetNamespace())
	force := false
	_, err = rc.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: manager, Force: &force})
	if err == nil {
		return nil
	}
	if !apierrors.IsConflict(err) {
		return fmt.Errorf("compose: applying %s %s in %s: %w", u.GetKind(), u.GetName(), ws, err)
	}
	c.recordConflict(ctx, r, ws, u, err)
	force = true
	if _, err := rc.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: manager, Force: &force}); err != nil {
		return fmt.Errorf("compose: applying %s %s in %s: %w", u.GetKind(), u.GetName(), ws, err)
	}
	return nil
}

func (c *Composer) recordConflict(ctx context.Context, r Root, ws string, u *unstructured.Unstructured, cause error) {
	message := fmt.Sprintf("%s %s: a field another manager set is reverted to the spec %s %s composes: %s", u.GetKind(), u.GetName(), r.Kind, r.Name, cause.Error())
	c.log.Warn("a direct edit of a composed child is reverted", "root", r.Owner(), "kind", u.GetKind(), "name", u.GetName(), "workspace", ws, "err", cause.Error())
	c.Event(ctx, r, ws, u.GetAPIVersion(), u.GetKind(), u.GetName(), "Warning", ReasonChildEditReverted, message)
}

func (c *Composer) Event(ctx context.Context, r Root, ws, apiVersion, kind, name, eventType, reason, message string) {
	ri, err := c.Client(ws, EventGVR)
	if err != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if len(message) > 1024 {
		message = message[:1024]
	}
	ev := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":         "v1",
		"kind":               "Event",
		"metadata":           map[string]any{"generateName": strings.ToLower(kind) + "-" + name + "-", "namespace": eventNamespace},
		"involvedObject":     map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name},
		"reason":             reason,
		"message":            message,
		"type":               eventType,
		"source":             map[string]any{"component": r.ManagedBy()},
		"reportingComponent": r.ManagedBy(),
		"firstTimestamp":     now,
		"lastTimestamp":      now,
		"count":              int64(1),
	}}
	if _, err := ri.Namespace(eventNamespace).Create(ctx, ev, metav1.CreateOptions{FieldManager: r.Manager()}); err != nil && ctx.Err() == nil {
		c.log.Warn("recording an event", "root", r.Owner(), "reason", reason, "err", err.Error())
	}
}

func (c *Composer) ApplyTyped(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, apiVersion, kind string, obj any) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	ws = workspaceOr(ws, r.Workspace)
	labels := r.Labels(kind)
	for k, v := range u.GetLabels() {
		labels[k] = v
	}
	u.SetLabels(labels)
	if ws == r.Workspace {
		u.SetOwnerReferences([]metav1.OwnerReference{r.OwnerRef()})
	}
	return c.Apply(ctx, r, ws, gvr, u)
}

func (c *Composer) Get(ctx context.Context, ws string, gvr schema.GroupVersionResource, name string) (*unstructured.Unstructured, error) {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return nil, err
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

func (c *Composer) GetComponent(ctx context.Context, ws, kind, name string) (ComponentView, bool, error) {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return ComponentView{}, false, err
	}
	u, err := c.Get(ctx, ws, ct.GVR, name)
	if apierrors.IsNotFound(err) {
		return ComponentView{Kind: kind, Name: name, Workspace: ws}, false, nil
	}
	if err != nil {
		return ComponentView{}, false, err
	}
	v, err := ComponentViewOf(ws, kind, u)
	return v, err == nil, err
}

func (c *Composer) ListComponents(ctx context.Context, ws, owner string) ([]ComponentView, error) {
	var out []ComponentView
	for _, ct := range ComponentTypes {
		ri, err := c.Client(ws, ct.GVR)
		if err != nil {
			return nil, err
		}
		list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			v, err := ComponentViewOf(ws, ct.Kind, &list.Items[i])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Composer) DeleteComponent(ctx context.Context, ws, kind, name string) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	ri, err := c.Client(ws, ct.GVR)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Composer) DeleteComponentsExcept(ctx context.Context, ws, owner string, keep []string) (int, error) {
	views, err := c.ListComponents(ctx, ws, owner)
	if err != nil {
		return 0, err
	}
	left := 0
	for _, v := range views {
		if slices.Contains(keep, ComponentKey(v.Kind, v.Name)) {
			continue
		}
		left++
		if err := c.DeleteComponent(ctx, ws, v.Kind, v.Name); err != nil {
			return left, err
		}
		c.log.Info("deleted a component the root object omits", "owner", owner, "kind", v.Kind, "name", v.Name, "workspace", ws)
	}
	return left, nil
}

func (c *Composer) RestartComponent(ctx context.Context, r Root, ws, kind, name string) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": ct.APIVersion(), "kind": ct.Kind}}
	u.SetName(name)
	if err := unstructured.SetNestedField(u.Object, time.Now().UTC().Format(time.RFC3339Nano), "spec", "restartedAt"); err != nil {
		return err
	}
	return c.applyAs(ctx, r, r.Manager()+"/restart", ws, ct.GVR, u)
}

func (c *Composer) Prune(ctx context.Context, ws string, gvr schema.GroupVersionResource, selector string, keep map[string]bool) error {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if keep[name] {
			continue
		}
		if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Composer) DeleteOwned(ctx context.Context, ws string, gvr schema.GroupVersionResource, owner string) (int, error) {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return 0, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, o := range list.Items {
		if o.GetDeletionTimestamp() == nil {
			if err := ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return len(list.Items), err
			}
		}
	}
	return len(list.Items), nil
}

func (c *Composer) DeleteAll(ctx context.Context, ws string, gvr schema.GroupVersionResource) (int, error) {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return 0, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, o := range list.Items {
		if o.GetDeletionTimestamp() == nil {
			if err := ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return len(list.Items), err
			}
		}
	}
	return len(list.Items), nil
}

func (c *Composer) SetSpecField(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, apiVersion, kind, name string, spec map[string]any) error {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "spec": spec}}
	u.SetName(name)
	return c.Apply(ctx, r, ws, gvr, u)
}

func jsonCopy(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

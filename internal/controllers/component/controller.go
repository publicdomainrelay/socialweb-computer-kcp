package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	svc "github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	Name = "component"

	Export = "applications"

	eventBuffer = 1024
)

type Options struct {
	Runtime *svc.Runtime

	Scope func(workspace string) bool

	Changed []func(svc.Key)

	Log *slog.Logger
}

type key struct {
	kind string

	cluster string

	name string
}

type Controller struct {
	runtime *svc.Runtime

	changed []func(svc.Key)

	log *slog.Logger

	mu sync.Mutex

	uids map[key]types.UID

	keys map[types.UID]key

	restarts map[key]string

	applied map[types.UID]string

	events map[string]chan ctlruntime.Request
}

func New(opts Options) (*Controller, error) {
	if opts.Runtime == nil {
		return nil, errors.New("component: a component runtime is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{runtime: opts.Runtime, changed: opts.Changed, log: log.With("component", "component-controller"), uids: map[key]types.UID{},
		keys: map[types.UID]key{}, restarts: map[key]string{}, applied: map[types.UID]string{}, events: map[string]chan ctlruntime.Request{}}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	c, err := New(opts)
	if err != nil {
		return nil, err
	}
	c.runtime.SetStatusCallback(c.statusChanged)
	errs := []error{
		register(m, c, opts.Scope, "Bidder", func(o *v1alpha1.Bidder) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "GuestControlPlane", func(o *v1alpha1.GuestControlPlane) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "Pds", func(o *v1alpha1.Pds) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "Requester", func(o *v1alpha1.Requester) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "GatewayAPI", func(o *v1alpha1.GatewayAPI) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "SshServer", func(o *v1alpha1.SshServer) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "Plc", func(o *v1alpha1.Plc) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "Relay", func(o *v1alpha1.Relay) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "IngressProxy", func(o *v1alpha1.IngressProxy) *v1alpha1.ComponentStatus { return &o.Status }),
		register(m, c, opts.Scope, "DispatcherTLS", func(o *v1alpha1.DispatcherTLS) *v1alpha1.ComponentStatus { return &o.Status }),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func register[T interface {
	ctlruntime.Object
	DeepCopy() T
}](m *ctlruntime.Manager, c *Controller, scope func(string) bool, kind string, status func(T) *v1alpha1.ComponentStatus) error {
	events := make(chan ctlruntime.Request, eventBuffer)
	c.mu.Lock()
	c.events[kind] = events
	c.mu.Unlock()
	r := &reconciler[T]{c: c, kind: kind, status: status}
	opts := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(events), ctlruntime.Gone(r.gone),
		ctlruntime.Watches(r.restarted)}
	if scope != nil {
		opts = append(opts, ctlruntime.InWorkspaces(scope))
	}
	if err := ctlruntime.For[T](m, Name+"-"+strings.ToLower(kind), r, opts...); err != nil {
		return fmt.Errorf("component: registering the %s controller: %w", kind, err)
	}
	return nil
}

type reconciler[T interface {
	ctlruntime.Object
	DeepCopy() T
}] struct {
	c *Controller

	kind string

	status func(T) *v1alpha1.ComponentStatus
}

func (r *reconciler[T]) Reconcile(ctx context.Context, req ctlruntime.Request, obj T) (ctlruntime.Result, error) {
	spec, err := specOf(r.kind, req, obj)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	k := key{kind: r.kind, cluster: req.Cluster, name: req.Name}
	r.c.remember(k, spec.UID, spec.RestartStamp())
	st, err := r.c.runtime.Reconcile(ctx, spec)
	if errors.Is(err, svc.ErrRuntimeNotStarted) {
		return ctlruntime.Result{}, err
	}
	if err != nil {
		r.c.log.Warn("the component did not start; the component runtime supervises it", "kind", r.kind, "workspace", req.Workspace,
			"name", req.Name, "err", err.Error())
	}
	if current, ok := r.c.runtime.Status(spec.UID); ok {
		st = current
	}
	if spec.Deleting {
		r.c.forget(k)
		return ctlruntime.Result{}, nil
	}
	next := obj.DeepCopy()
	*r.status(next) = st.APIStatus()
	if err := req.ApplyStatus(ctx, next); err != nil {
		return ctlruntime.Result{}, err
	}
	r.c.mu.Lock()
	r.c.applied[spec.UID] = fingerprint(st)
	r.c.mu.Unlock()
	return ctlruntime.Result{}, nil
}

func (r *reconciler[T]) gone(ctx context.Context, req ctlruntime.Request) {
	k := key{kind: r.kind, cluster: req.Cluster, name: req.Name}
	uid, ok := r.c.forget(k)
	if !ok {
		return
	}
	if err := r.c.runtime.Remove(ctx, uid); err != nil {
		r.c.log.Warn("stopping a deleted component", "kind", r.kind, "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
}

func (r *reconciler[T]) restarted(_ context.Context, src ctlruntime.Request, obj T) []ctlruntime.Request {
	stamp := obj.GetAnnotations()[svc.AnnotationRestartAt]
	if stamp == "" {
		return nil
	}
	r.c.mu.Lock()
	seen := r.c.restarts[key{kind: r.kind, cluster: src.Cluster, name: obj.GetName()}]
	r.c.mu.Unlock()
	if seen == stamp {
		return nil
	}
	return []ctlruntime.Request{{Cluster: src.Cluster, Namespace: obj.GetNamespace(), Name: obj.GetName()}}
}

func (c *Controller) remember(k key, uid types.UID, stamp string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.uids[k]; ok && old != uid {
		delete(c.keys, old)
	}
	c.uids[k], c.keys[uid], c.restarts[k] = uid, k, stamp
}

func (c *Controller) forget(k key) (types.UID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uid, ok := c.uids[k]
	delete(c.uids, k)
	delete(c.restarts, k)
	if ok {
		delete(c.keys, uid)
		delete(c.applied, uid)
	}
	return uid, ok
}

func (c *Controller) statusChanged(ctx context.Context, spec svc.ComponentSpec, status svc.ComponentStatus) error {
	for _, fn := range c.changed {
		fn(svc.Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name})
	}
	c.mu.Lock()
	k, ok := c.keys[spec.UID]
	events := c.events[spec.Kind]
	same := c.applied[spec.UID] == fingerprint(status)
	c.mu.Unlock()
	if !ok || events == nil || same {
		return nil
	}
	select {
	case events <- ctlruntime.Request{Cluster: k.cluster, Namespace: spec.Namespace, Name: k.name}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func fingerprint(status svc.ComponentStatus) string {
	st := status.APIStatus()
	for i := range st.Conditions {
		st.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return string(raw)
}

func specOf[T ctlruntime.Object](kind string, req ctlruntime.Request, obj T) (svc.ComponentSpec, error) {
	spec := svc.ComponentSpec{
		UID:         obj.GetUID(),
		Group:       v1alpha1.GroupName,
		Kind:        kind,
		Name:        obj.GetName(),
		Namespace:   obj.GetNamespace(),
		Workspace:   req.Workspace,
		Generation:  obj.GetGeneration(),
		Deleting:    !obj.GetDeletionTimestamp().IsZero(),
		Annotations: maps.Clone(obj.GetAnnotations()),
		Labels:      maps.Clone(obj.GetLabels()),
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return spec, fmt.Errorf("component: encoding %s/%s: %w", kind, spec.Name, err)
	}
	var doc struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return spec, fmt.Errorf("component: the spec of %s/%s: %w", kind, spec.Name, err)
	}
	if len(doc.Spec) == 0 || string(doc.Spec) == "null" {
		return spec, nil
	}
	var common struct {
		Suspend       bool   `json:"suspend"`
		RestartPolicy string `json:"restartPolicy"`
		StateDir      string `json:"stateDir"`
	}
	if err := json.Unmarshal(doc.Spec, &common); err != nil {
		return spec, fmt.Errorf("component: the spec of %s/%s: %w", kind, spec.Name, err)
	}
	spec.Suspend, spec.RestartPolicy, spec.StateDir = common.Suspend, svc.RestartPolicy(common.RestartPolicy), common.StateDir
	spec.Spec = svc.RawSpec(doc.Spec)
	return spec, nil
}

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

const scopeMissRetries = 10

type Object interface {
	metav1.Object
	kruntime.Object
}

type Result struct {
	RequeueAfter time.Duration

	Waiting bool
}

type Reconciler[T Object] interface {
	Reconcile(ctx context.Context, req Request, obj T) (Result, error)
}

type ReconcilerFunc[T Object] func(ctx context.Context, req Request, obj T) (Result, error)

func (f ReconcilerFunc[T]) Reconcile(ctx context.Context, req Request, obj T) (Result, error) {
	return f(ctx, req, obj)
}

type Request struct {
	Cluster string

	Workspace string

	Namespace string

	Name string

	scope *scope
}

type scope struct {
	client client.Client

	scheme *kruntime.Scheme

	fieldManager string

	finalizer string

	sharedStatus bool

	log *slog.Logger
}

type Option func(*options)

type options struct {
	export string

	fieldManager string

	finalizer string

	owns []client.Object

	watches []watchInput

	only func(client.Object) bool

	inWorkspace func(context.Context, string) bool

	gone func(context.Context, Request)

	events []<-chan Request

	sharedStatus bool

	backoffBase time.Duration

	backoffMax time.Duration

	workers int

	err error
}

type watchInput struct {
	object client.Object

	export string

	mapFunc func(ctx context.Context, src Request, obj client.Object) []Request
}

func Export(name string) Option {
	return func(o *options) { o.export = strings.TrimSpace(name) }
}

func FieldManager(name string) Option {
	return func(o *options) { o.fieldManager = strings.TrimSpace(name) }
}

func Finalizer(name string) Option {
	return func(o *options) { o.finalizer = strings.TrimSpace(name) }
}

func Backoff(base, max time.Duration) Option {
	return func(o *options) { o.backoffBase, o.backoffMax = base, max }
}

func Workers(n int) Option {
	return func(o *options) { o.workers = n }
}

func SharedStatus() Option {
	return func(o *options) { o.sharedStatus = true }
}

func Events(ch <-chan Request) Option {
	return func(o *options) {
		if ch != nil {
			o.events = append(o.events, ch)
		}
	}
}

func InWorkspaces(keep func(workspace string) bool) Option {
	return func(o *options) { o.inWorkspace = inWorkspaces(keep) }
}

func InWorkspacesContext(keep func(ctx context.Context, workspace string) bool) Option {
	return func(o *options) { o.inWorkspace = keep }
}

func inWorkspaces(keep func(workspace string) bool) func(context.Context, string) bool {
	if keep == nil {
		return nil
	}
	return func(_ context.Context, workspace string) bool { return keep(workspace) }
}

func Gone(fn func(ctx context.Context, req Request)) Option {
	return func(o *options) { o.gone = fn }
}

func Only[T Object](keep func(T) bool) Option {
	return func(o *options) {
		if keep == nil {
			return
		}
		o.only = func(obj client.Object) bool {
			typed, ok := obj.(T)
			return ok && keep(typed)
		}
	}
}

func Owns[C Object]() Option {
	return func(o *options) {
		obj, err := newObject[C]()
		if err != nil {
			o.err = errors.Join(o.err, err)
			return
		}
		o.owns = append(o.owns, obj)
	}
}

func Watches[R Object](mapFunc func(ctx context.Context, src Request, obj R) []Request) Option {
	return WatchesIn("", mapFunc)
}

func WatchesIn[R Object](export string, mapFunc func(ctx context.Context, src Request, obj R) []Request) Option {
	return func(o *options) {
		obj, err := newObject[R]()
		if err != nil {
			o.err = errors.Join(o.err, err)
			return
		}
		if mapFunc == nil {
			o.err = errors.Join(o.err, fmt.Errorf("runtime: the watch of %T has no map func", obj))
			return
		}
		o.watches = append(o.watches, watchInput{object: obj, export: strings.TrimSpace(export), mapFunc: func(ctx context.Context, src Request, obj client.Object) []Request {
			typed, ok := obj.(R)
			if !ok {
				return nil
			}
			return mapFunc(ctx, src, typed)
		}})
	}
}

func For[T Object](m *Manager, name string, r Reconciler[T], opts ...Option) error {
	name = strings.TrimSpace(name)
	if m == nil {
		return errors.New("runtime: a manager is required")
	}
	if name == "" {
		return errors.New("runtime: a controller needs a name")
	}
	if r == nil {
		return fmt.Errorf("runtime: the controller %s has no reconciler", name)
	}
	o := options{fieldManager: FieldManagerPrefix + name, backoffBase: DefaultBackoffBase, backoffMax: DefaultBackoffMax, workers: 1}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.err != nil {
		return fmt.Errorf("runtime: the controller %s: %w", name, o.err)
	}
	if o.export == "" {
		return fmt.Errorf("runtime: the controller %s names no APIExport", name)
	}
	o.workers = opt.Positive(o.workers, 1)
	if o.inWorkspace == nil {
		o.inWorkspace = inWorkspaces(m.workspaces)
	}
	proto, err := newObject[T]()
	if err != nil {
		return fmt.Errorf("runtime: the controller %s: %w", name, err)
	}
	m.addExport(o.export)
	inExport := exportFilter(o.export)
	limiter := newBackoff(o.backoffBase, o.backoffMax)
	log := m.log.With("controller", name)
	b := mcbuilder.ControllerManagedBy(m.mgr).Named(name).
		For(proto, inExport, mcbuilder.WithPredicates(specOrDeletion())).
		WithOptions(controller.TypedOptions[mcreconcile.Request]{MaxConcurrentReconciles: o.workers, RateLimiter: limiter,
			SkipNameValidation: new(true)})
	for _, owned := range o.owns {
		b = b.Owns(owned, inExport)
	}
	for _, w := range o.watches {
		source := inExport
		if w.export != "" && w.export != o.export {
			m.addExport(w.export)
			source = exportFilter(w.export)
		}
		b = b.Watches(w.object, m.mapHandler(o.export, w.mapFunc), source)
	}
	for _, ch := range o.events {
		b = b.WatchesRawSource(m.eventSource(o.export, ch))
	}
	a := &adapter[T]{m: m, name: name, r: r, limiter: limiter, log: log, only: o.only, inWorkspace: o.inWorkspace, gone: o.gone,
		scope: scope{scheme: m.scheme, fieldManager: o.fieldManager, finalizer: o.finalizer, sharedStatus: o.sharedStatus, log: log}}
	if err := b.Complete(a); err != nil {
		return fmt.Errorf("runtime: building the controller %s: %w", name, err)
	}
	log.Info("the controller is registered", "kind", fmt.Sprintf("%T", proto), "export", o.export, "fieldManager", o.fieldManager)
	return nil
}

func (m *Manager) mapHandler(export string, mapFunc func(ctx context.Context, src Request, obj client.Object) []Request) mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request] {
	return func(cn multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		_, lc := splitCluster(string(cn))
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
			workspace, err := m.path(ctx, lc)
			if err != nil {
				m.log.Warn("a watched object is not mapped: its workspace path does not resolve", "export", export, "cluster", lc,
					"name", obj.GetName(), "err", err.Error())
				return nil
			}
			src := Request{Cluster: lc, Workspace: workspace, Namespace: obj.GetNamespace(), Name: obj.GetName()}
			var out []mcreconcile.Request
			for _, r := range mapFunc(ctx, src, obj) {
				cluster := lc
				if r.Cluster != "" {
					cluster = r.Cluster
				}
				target := multicluster.ClusterName(export + clusterSeparator + cluster)
				out = append(out, mcreconcile.Request{ClusterName: target,
					Request: reconcile.Request{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: r.Name}}})
			}
			return out
		})
	}
}

func (m *Manager) eventSource(export string, ch <-chan Request) source.TypedSource[mcreconcile.Request] {
	return source.TypedFunc[mcreconcile.Request](func(ctx context.Context, q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) error {
		m.sourcesWG.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case r, ok := <-ch:
					if !ok {
						return
					}
					q.Add(mcreconcile.Request{ClusterName: multicluster.ClusterName(export + clusterSeparator + r.Cluster),
						Request: reconcile.Request{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: r.Name}}})
				}
			}
		})
		return nil
	})
}

func exportFilter(export string) mcbuilder.EngageOptions {
	prefix := export + clusterSeparator
	return mcbuilder.WithClusterFilter(func(cn multicluster.ClusterName, _ cluster.Cluster) bool {
		return strings.HasPrefix(string(cn), prefix)
	})
}

func specOrDeletion() predicate.Predicate {
	return predicate.Or(predicate.GenerationChangedPredicate{}, predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
	})
}

type adapter[T Object] struct {
	m *Manager

	name string

	r Reconciler[T]

	limiter *backoff

	log *slog.Logger

	only func(client.Object) bool

	inWorkspace func(context.Context, string) bool

	gone func(context.Context, Request)

	scope scope
}

func (a *adapter[T]) Reconcile(ctx context.Context, mcreq mcreconcile.Request) (reconcile.Result, error) {
	res, err := a.reconcile(ctx, mcreq)
	if err != nil && a.m.stopping(ctx) {
		a.log.Debug("the reconcile ended with the shutdown", "cluster", mcreq.ClusterName, "name", mcreq.Name, "err", err.Error())
		return reconcile.Result{}, nil
	}
	return res, err
}

func (a *adapter[T]) reconcile(ctx context.Context, mcreq mcreconcile.Request) (reconcile.Result, error) {
	_, lc := splitCluster(string(mcreq.ClusterName))
	cl, err := a.m.mgr.GetCluster(ctx, mcreq.ClusterName)
	if err != nil {
		return reconcile.Result{}, err
	}
	workspace, err := a.m.path(ctx, lc)
	if err != nil {
		return reconcile.Result{}, a.failed(ctx, mcreq, lc, mcreq.Name, err)
	}
	if a.inWorkspace != nil && !a.inWorkspace(ctx, workspace) {
		return a.outOfScope(mcreq, workspace), nil
	}
	obj, err := newObject[T]()
	if err != nil {
		return reconcile.Result{}, reconcile.TerminalError(err)
	}
	s := a.scope
	s.client = cl.GetClient()
	req := Request{Cluster: lc, Workspace: workspace, Namespace: mcreq.Namespace, Name: mcreq.Name, scope: &s}
	if err := cl.GetClient().Get(ctx, mcreq.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			if a.gone != nil {
				a.gone(ctx, req)
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, a.failed(ctx, mcreq, lc, mcreq.Name, err)
	}
	if a.only != nil && !a.only(obj) {
		return reconcile.Result{}, nil
	}
	log := a.log.With("cluster", lc, "workspace", req.Workspace, "name", req.Name)
	s.log = log
	if s.finalizer != "" && obj.GetDeletionTimestamp().IsZero() && !controllerutil.ContainsFinalizer(obj, s.finalizer) {
		if err := req.applyFinalizer(ctx, obj, true); err != nil {
			return reconcile.Result{}, a.failed(ctx, mcreq, lc, req.Name, err)
		}
	}
	gvk, err := apiutil.GVKForObject(obj, cl.GetScheme())
	if err != nil {
		return reconcile.Result{}, reconcile.TerminalError(err)
	}
	ctx = bus.WithCallerLabels(ctx, commonv1.ObjectRef{Group: gvk.Group, Kind: gvk.Kind, Name: obj.GetName(), Workspace: req.Workspace}, obj.GetLabels())
	log.Debug("reconciling")
	res, err := a.r.Reconcile(ctx, req, obj)
	if err != nil {
		return reconcile.Result{}, a.failed(ctx, mcreq, req.Workspace, req.Name, err)
	}
	if s.finalizer != "" && !res.Waiting && !obj.GetDeletionTimestamp().IsZero() && controllerutil.ContainsFinalizer(obj, s.finalizer) {
		if err := req.applyFinalizer(ctx, obj, false); err != nil {
			return reconcile.Result{}, a.failed(ctx, mcreq, req.Workspace, req.Name, err)
		}
	}
	return reconcile.Result{RequeueAfter: res.RequeueAfter}, nil
}

func (a *adapter[T]) outOfScope(item mcreconcile.Request, workspace string) reconcile.Result {
	misses := a.limiter.NumRequeues(item)
	if misses >= scopeMissRetries {
		a.log.Debug("the workspace stays outside the controller's scope; the request is dropped", "workspace", workspace,
			"name", item.Name, "misses", misses)
		return reconcile.Result{}
	}
	a.log.Debug("the workspace is not in the controller's scope yet; it requeues with backoff", "workspace", workspace,
		"name", item.Name, "misses", misses+1)
	return reconcile.Result{Requeue: true} //nolint:staticcheck
}

func (a *adapter[T]) failed(ctx context.Context, item mcreconcile.Request, workspace, name string, err error) error {
	if a.m.stopping(ctx) {
		return err
	}
	failures, retryIn := a.limiter.peek(item)
	a.log.Warn("the reconcile failed; it retries with backoff", "workspace", workspace, "name", name,
		"failures", failures, "retryIn", retryIn.String(), "err", err.Error())
	return err
}

func newObject[T Object]() (T, error) {
	var zero T
	t := reflect.TypeOf(zero)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return zero, fmt.Errorf("runtime: %v is not a pointer to a struct", t)
	}
	obj, ok := reflect.New(t.Elem()).Interface().(T)
	if !ok {
		return zero, fmt.Errorf("runtime: %v does not implement the object interface", t)
	}
	return obj, nil
}

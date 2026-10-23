package atprotorecordwatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecord"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Name = "atprotorecordwatch"

	Export = "marketidentities"

	EventsExport = "events"

	RepoExport = "applications"

	FieldManager = ctlruntime.FieldManagerPrefix + Name

	Kind = events.RecordWatchKind

	RepoKind = eventstream.AtprotoRepoKind

	LocalRepoKind = "LocalRepo"

	RemoteRepoKind = "RemoteRepo"

	VerifyInterval = 5 * time.Minute

	VerifyRetry = 30 * time.Second

	requestQueue = 256
)

type Children interface {
	ApplyEventStream(ctx context.Context, workspace string, obj *eventsv1.EventStream) error

	ApplyEventSource(ctx context.Context, workspace string, obj *eventsv1.EventSource) error

	ApplyPdsEventSource(ctx context.Context, workspace string, obj *eventsv1.PdsEventSource) error

	EventStreams(ctx context.Context, workspace, watch string) ([]eventsv1.EventStream, error)

	DeleteEventStream(ctx context.Context, workspace, name string) error
}

type Repos interface {
	AtprotoRepo(ctx context.Context, workspace, name string) (*v1alpha1.AtprotoRepo, error)

	LocalRepo(ctx context.Context, workspace, name string) (*v1alpha1.LocalRepo, error)

	RemoteRepo(ctx context.Context, workspace, name string) (*v1alpha1.RemoteRepo, error)
}

type ValueReader func(ctx context.Context, workspace string, from v1alpha1.AtprotoValueFrom) (map[string]any, error)

type Options struct {
	Bus *bus.Bus

	Backend *eventstream.Watches

	Children Children

	Repos Repos

	Values ValueReader

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	backend *eventstream.Watches

	children Children

	repos Repos

	values ValueReader

	log *slog.Logger

	refs *binding.Table[struct{}]

	requests chan ctlruntime.Request

	mu sync.Mutex

	watches map[string]*watch
}

type watch struct {
	uid types.UID

	cluster string

	name string

	matches []eventstream.Match

	matched bool

	status v1alpha1.AtprotoRecordWatchStatus

	verifiedGeneration int64

	verifyAt time.Time

	sink *eventstream.Sink
}

func New(opts Options) (*Controller, error) {
	if opts.Backend == nil {
		return nil, errors.New("atprotorecordwatch: a record watch backend is required")
	}
	if opts.Children == nil {
		return nil, errors.New("atprotorecordwatch: an events children writer is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, backend: opts.Backend, children: opts.Children, repos: opts.Repos, values: opts.Values,
		log: log.With("component", "atprotorecordwatch-controller"), refs: binding.New[struct{}](),
		requests: make(chan ctlruntime.Request, requestQueue), watches: map[string]*watch{}}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme, eventsv1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	ctl.Provide()
	options := []ctlruntime.Option{ctlruntime.Export(Export),
		ctlruntime.WatchesIn(EventsExport, func(_ context.Context, _ ctlruntime.Request, obj *eventsv1.EventStream) []ctlruntime.Request {
			return owner(obj)
		}),
		ctlruntime.WatchesIn(EventsExport, func(_ context.Context, _ ctlruntime.Request, obj *eventsv1.EventSource) []ctlruntime.Request {
			return owner(obj)
		}),
		ctlruntime.WatchesIn(RepoExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRepo) []ctlruntime.Request {
			return ctl.users(src.Workspace, RepoKind, obj.Name)
		}),
		ctlruntime.WatchesIn(RepoExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.LocalRepo) []ctlruntime.Request {
			return ctl.users(src.Workspace, LocalRepoKind, obj.Name)
		}),
		ctlruntime.WatchesIn(RepoExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.RemoteRepo) []ctlruntime.Request {
			return ctl.users(src.Workspace, RemoteRepoKind, obj.Name)
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.MarketIdentity) []ctlruntime.Request {
			return ctl.users(src.Workspace, atprotorecord.MarketIdentityKind, obj.Name)
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRecord) []ctlruntime.Request {
			return ctl.users(src.Workspace, atprotorecord.Kind, obj.Name)
		}),
		ctlruntime.Events(ctl.requests),
		ctlruntime.Gone(ctl.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.AtprotoRecordWatch](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("atprotorecordwatch: registering the AtprotoRecordWatch controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Provide() {
	bus.Provide[events.Sink](c.bus, schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind}, events.CapabilityDeliver, c.open)
}

func (c *Controller) open(_ context.Context, to commonv1.ObjectRef, _ []string) (events.Sink, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.watches[binding.Key(to.Workspace, to.Name)]
	if w == nil {
		return nil, fmt.Errorf("%w: the AtprotoRecordWatch %s is not reconciled", bus.ErrNoProvider, to.String())
	}
	return w.sink, nil
}

func owner(obj metav1.Object) []ctlruntime.Request {
	if name := strings.TrimSpace(obj.GetLabels()[eventstream.RecordWatchLabel]); name != "" {
		return []ctlruntime.Request{{Name: name}}
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == Kind && strings.HasPrefix(ref.APIVersion, v1alpha1.GroupName+"/") {
			return []ctlruntime.Request{{Name: ref.Name}}
		}
	}
	return nil
}

func (c *Controller) users(workspace, kind, name string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.refs.Users(binding.Key(workspace, kind, name)) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) bind(req ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) *watch {
	key := binding.Key(req.Workspace, req.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.watches[key]
	if w == nil {
		w = &watch{name: req.Name, cluster: req.Cluster}
		w.sink = c.backend.NewSink(req.Name, func() ([]eventstream.Match, bool) {
			c.mu.Lock()
			defer c.mu.Unlock()
			return w.matches, w.matched
		}, func() {
			c.mu.Lock()
			target := ctlruntime.Request{Cluster: w.cluster, Name: w.name}
			c.mu.Unlock()
			c.requests <- target
		})
		c.watches[key] = w
	}
	w.cluster = req.Cluster
	if w.uid != obj.UID {
		w.uid = obj.UID
		w.status = v1alpha1.AtprotoRecordWatchStatus{Records: slices.Clone(obj.Status.Records), Deleted: slices.Clone(obj.Status.Deleted),
			Cursors: slices.Clone(obj.Status.Cursors)}
		w.matches, w.matched = nil, false
		w.verifiedGeneration, w.verifyAt = 0, time.Time{}
		w.sink.Take()
	}
	return w
}

func (c *Controller) setMatches(w *watch, matches []eventstream.Match, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.matches, w.matched = matches, ok
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	c.release(req)
	streams, err := c.children.EventStreams(ctx, req.Workspace, req.Name)
	if err != nil {
		c.log.Warn("listing the EventStreams of a gone AtprotoRecordWatch", "workspace", req.Workspace, "name", req.Name, "err", err.Error())
		return
	}
	for _, stream := range streams {
		if err := c.children.DeleteEventStream(ctx, req.Workspace, stream.Name); err != nil && !apierrors.IsNotFound(err) {
			c.log.Warn("collecting an EventStream", "workspace", req.Workspace, "name", stream.Name, "err", err.Error())
		}
	}
}

func (c *Controller) release(req ctlruntime.Request) {
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
	key := binding.Key(req.Workspace, req.Name)
	c.mu.Lock()
	w := c.watches[key]
	delete(c.watches, key)
	c.mu.Unlock()
	if w != nil {
		w.sink.Close()
	}
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) (ctlruntime.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		c.gone(ctx, req)
		return ctlruntime.Result{}, nil
	}
	w := c.bind(req, obj)
	w.status = eventstream.MergeStatus(w.status, w.sink.Take())
	generation := obj.Generation
	repo, repoKeys, err := c.repo(ctx, req.Workspace, obj.Spec)
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, &struct{}{},
		append(repoKeys, matchKeys(req.Workspace, obj.Spec)...)...)
	if err == nil {
		var plans []eventstream.SourcePlan
		if plans, err = eventstream.PlanSources(req.Workspace, obj.Name, obj.Spec, repo); err == nil {
			return c.sync(ctx, req, obj, w, plans)
		}
	}
	var transient transientError
	if errors.As(err, &transient) {
		return ctlruntime.Result{}, errors.Join(err, c.apply(ctx, req, obj, w, failedConditions("ReconcileFailed", err, generation)))
	}
	return ctlruntime.Result{}, c.apply(ctx, req, obj, w, failedConditions("InvalidSpec", err, generation))
}

func (c *Controller) sync(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch, w *watch,
	plans []eventstream.SourcePlan) (ctlruntime.Result, error) {
	generation := obj.Generation
	unwatched := false
	matches, err := eventstream.ResolveMatches(ctx, obj.Spec.Match, func(ctx context.Context, from v1alpha1.AtprotoValueFrom) (map[string]any, error) {
		if object, ok, err := atprotorecord.ReadOwn(ctx, req, from); ok {
			return object, err
		}
		unwatched = true
		if c.values == nil {
			return nil, fmt.Errorf("no reader for %s %s", from.Kind, from.Name)
		}
		return c.values(ctx, cmp.Or(strings.TrimSpace(from.Workspace), req.Workspace), from)
	})
	if err != nil {
		c.setMatches(w, nil, false)
		pending := []metav1.Condition{
			conds.New(eventstream.RecordWatchConditionReady, false, "MatchPending", err.Error(), generation),
			conds.New(eventstream.RecordWatchConditionProgressing, true, "MatchPending", err.Error(), generation),
			conds.New(eventstream.RecordWatchConditionFailed, false, "NoError", err.Error(), generation),
			conds.New(eventstream.RecordWatchConditionVerified, false, "MatchPending", err.Error(), generation),
		}
		applyErr := c.apply(ctx, req, obj, w, pending)
		if unwatched {
			return ctlruntime.Result{}, errors.Join(err, applyErr)
		}
		return ctlruntime.Result{}, applyErr
	}
	c.setMatches(w, matches, true)
	ownerRef := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: Kind, Name: obj.Name, UID: obj.UID}
	wanted := map[string]bool{}
	for _, plan := range plans {
		wanted[plan.Name] = true
		if err := c.applyChildren(ctx, req.Workspace, ownerRef, plan); err != nil {
			return ctlruntime.Result{}, errors.Join(err, c.apply(ctx, req, obj, w, failedConditions("ReconcileFailed", err, generation)))
		}
	}
	listed, err := c.children.EventStreams(ctx, req.Workspace, obj.Name)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	streams := map[string]eventsv1.EventStream{}
	for _, stream := range listed {
		if wanted[stream.Name] {
			streams[stream.Name] = stream
			continue
		}
		if err := c.children.DeleteEventStream(ctx, req.Workspace, stream.Name); err != nil && !apierrors.IsNotFound(err) {
			c.log.Warn("collecting a stale EventStream", "workspace", req.Workspace, "name", stream.Name, "err", err.Error())
		}
	}
	conditions := eventstream.StreamConditions(plans, streams, generation)
	verified, reason, message := c.verify(ctx, obj, w, matches)
	if ready := meta.FindStatusCondition(conditions, eventstream.RecordWatchConditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		verified, reason, message = false, "StreamNotReady", "the EventStreams are starting"
	}
	conditions = append(conditions, conds.New(eventstream.RecordWatchConditionVerified, verified, reason, message, generation))
	if err := c.apply(ctx, req, obj, w, conditions); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{RequeueAfter: max(time.Until(w.verifyAt), time.Second)}, nil
}

func (c *Controller) verify(ctx context.Context, obj *v1alpha1.AtprotoRecordWatch, w *watch, matches []eventstream.Match) (bool, string, string) {
	if w.verifiedGeneration == obj.Generation && time.Now().Before(w.verifyAt) {
		if existing := meta.FindStatusCondition(obj.Status.Conditions, eventstream.RecordWatchConditionVerified); existing != nil &&
			existing.Reason != "StreamNotReady" && existing.Reason != "MatchPending" {
			return existing.Status == metav1.ConditionTrue, existing.Reason, existing.Message
		}
		return true, "Verified", "the watched records are verified"
	}
	result := c.backend.Verify(ctx, w.status.Records, matches)
	w.status.Records = result.Records
	w.verifiedGeneration = obj.Generation
	if !result.Verified() {
		w.verifyAt = time.Now().Add(VerifyRetry)
		return false, "VerifyFailed", result.Message()
	}
	w.verifyAt = time.Now().Add(VerifyInterval)
	return true, "Verified", result.Message()
}

func (c *Controller) applyChildren(ctx context.Context, workspace string, ownerRef metav1.OwnerReference, plan eventstream.SourcePlan) error {
	if backend, abstract, ok := eventstream.PdsSourcesFor(ownerRef, workspace, plan); ok {
		if err := c.children.ApplyPdsEventSource(ctx, workspace, backend); err != nil {
			return fmt.Errorf("applying PdsEventSource %s: %w", backend.Name, err)
		}
		if err := c.children.ApplyEventSource(ctx, workspace, abstract); err != nil {
			return fmt.Errorf("applying EventSource %s: %w", abstract.Name, err)
		}
	}
	stream := eventstream.EventStreamFor(ownerRef, workspace, plan)
	if err := c.children.ApplyEventStream(ctx, workspace, stream); err != nil {
		return fmt.Errorf("applying EventStream %s: %w", stream.Name, err)
	}
	return nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch, w *watch, conditions []metav1.Condition) error {
	next := obj.DeepCopy()
	next.Status = v1alpha1.AtprotoRecordWatchStatus{ObservedGeneration: obj.Generation, Records: w.status.Records, Deleted: w.status.Deleted,
		Cursors: w.status.Cursors, Conditions: slices.Clone(obj.Status.Conditions)}
	for _, condition := range conditions {
		conds.Set(&next.Status.Conditions, condition.ObservedGeneration, condition.Type, condition.Status == metav1.ConditionTrue,
			condition.Reason, condition.Message)
	}
	return req.ApplyStatus(ctx, next)
}

func failedConditions(reason string, err error, generation int64) []metav1.Condition {
	return []metav1.Condition{
		conds.New(eventstream.RecordWatchConditionReady, false, reason, err.Error(), generation),
		conds.New(eventstream.RecordWatchConditionProgressing, false, reason, err.Error(), generation),
		conds.New(eventstream.RecordWatchConditionFailed, true, reason, err.Error(), generation),
		conds.New(eventstream.RecordWatchConditionVerified, false, reason, err.Error(), generation),
	}
}

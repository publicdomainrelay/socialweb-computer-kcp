package eventstream

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecordwatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventsource"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/delivery"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/debounce"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

const (
	Name = "eventstream"

	Export = "events"

	Finalizer = "events.socialweb.computer/eventstream"

	ReasonInvalidSpec = "InvalidSpec"

	ReasonSourceMissing = "SourceMissing"

	ReasonSourceUnbound = "SourceUnbound"

	ReasonResolveFailed = "ResolveFailed"

	ReasonSourceNotReady = "SourceNotReady"

	ReasonSinkNotReady = "SinkNotReady"

	ReasonSuspended = "Suspended"

	statusTimeout = 10 * time.Second

	boundQueue = 64
)

type Options struct {
	Bus *bus.Bus

	Cursors delivery.CursorStore

	FlushDelay time.Duration

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	cursors delivery.CursorStore

	flushDelay time.Duration

	log *slog.Logger

	root context.Context

	cancelRoot context.CancelFunc

	mu sync.Mutex

	streams map[string]*stream

	sources refIndex

	sinks refIndex

	bound chan ctlruntime.Request
}

type refIndex struct {
	by map[string]map[string]ctlruntime.Request

	of map[string]string
}

func newRefIndex() refIndex {
	return refIndex{by: map[string]map[string]ctlruntime.Request{}, of: map[string]string{}}
}

func (x refIndex) users(target string) []ctlruntime.Request {
	out := make([]ctlruntime.Request, 0, len(x.by[target]))
	for _, r := range x.by[target] {
		out = append(out, r)
	}
	return out
}

func (x refIndex) set(streamKey, target string, req ctlruntime.Request) {
	if prior, ok := x.of[streamKey]; ok {
		delete(x.by[prior], streamKey)
		if len(x.by[prior]) == 0 {
			delete(x.by, prior)
		}
		delete(x.of, streamKey)
	}
	if target == "" {
		return
	}
	if x.by[target] == nil {
		x.by[target] = map[string]ctlruntime.Request{}
	}
	x.by[target][streamKey] = ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	x.of[streamKey] = target
}

func Register(ctx context.Context, m *ctlruntime.Manager, opts Options) (*Controller, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	root, cancel := context.WithCancel(ctx)
	c := &Controller{bus: opts.Bus, cursors: opts.Cursors, flushDelay: opts.FlushDelay, log: log.With("component", "eventstream-controller"),
		root: root, cancelRoot: cancel, streams: map[string]*stream{}, sources: newRefIndex(), sinks: newRefIndex(),
		bound: make(chan ctlruntime.Request, boundQueue)}
	c.wakeOnBind()
	err := ctlruntime.For[*eventsv1.EventStream](m, Name, c, ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer), ctlruntime.Events(c.bound),
		ctlruntime.Watches[*eventsv1.EventSource](func(_ context.Context, src ctlruntime.Request, _ *eventsv1.EventSource) []ctlruntime.Request {
			return c.streamsOf(key(src.Workspace, src.Name))
		}))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("eventstream: registering the EventStream controller: %w", err)
	}
	return c, nil
}

func (c *Controller) wakeOnBind() {
	c.bus.Subscribe(func(k bus.Key) {
		var reqs []ctlruntime.Request
		switch k.Kind {
		case eventsv1.EventSourceKind:
			reqs = c.streamsOf(key(k.Workspace, k.Name))
		case events.RecordWatchKind:
			reqs = c.streamsTo(key(k.Workspace, k.Name))
		}
		for _, req := range reqs {
			select {
			case c.bound <- req:
			default:
			}
		}
	})
}

func (c *Controller) Run(ctx context.Context) error {
	<-ctx.Done()
	c.cancelRoot()
	c.mu.Lock()
	streams := make([]*stream, 0, len(c.streams))
	for k, s := range c.streams {
		streams = append(streams, s)
		delete(c.streams, k)
	}
	c.mu.Unlock()
	for _, s := range streams {
		s.stop(true)
	}
	return nil
}

func key(workspace, name string) string {
	return strings.TrimSpace(workspace) + "/" + strings.TrimSpace(name)
}

func (c *Controller) streamsOf(source string) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sources.users(source)
}

func (c *Controller) streamsTo(sink string) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sinks.users(sink)
}

func (c *Controller) indexSource(streamKey, source string, req ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources.set(streamKey, source, req)
}

func (c *Controller) indexSink(streamKey, sink string, req ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sinks.set(streamKey, sink, req)
}

func SourceRef(workspace string, ref commonv1.ObjectRef) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: eventsv1.EventSourceKind, Name: strings.TrimSpace(ref.Name), Workspace: ref.InWorkspace(workspace)}
}

func SinkRef(workspace string, sink eventsv1.EventStreamSink) (commonv1.ObjectRef, error) {
	name := strings.TrimSpace(sink.Name)
	if name == "" {
		return commonv1.ObjectRef{}, errors.New("spec.sink needs a name")
	}
	switch sink.Kind {
	case eventsv1.EventStreamSinkRecordWatch:
		return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: events.RecordWatchKind, Name: name,
			Workspace: cmp.Or(strings.TrimSpace(sink.Namespace), workspace)}, nil
	case eventsv1.EventStreamSinkHandler:
		return commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: events.HandlerKind, Name: name, Workspace: workspace}, nil
	default:
		return commonv1.ObjectRef{}, fmt.Errorf("unknown sink kind %q", sink.Kind)
	}
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, es *eventsv1.EventStream) (ctlruntime.Result, error) {
	k := key(req.Workspace, es.Name)
	if !es.DeletionTimestamp.IsZero() {
		c.indexSource(k, "", req)
		c.indexSink(k, "", req)
		c.stop(k, false)
		return ctlruntime.Result{}, nil
	}
	if strings.TrimSpace(es.Spec.SourceRef.Name) == "" {
		c.stop(k, true)
		return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhaseFailed, false, true, ReasonInvalidSpec, "spec.sourceRef needs a name")
	}
	src := SourceRef(req.Workspace, es.Spec.SourceRef)
	c.indexSource(k, key(src.Workspace, src.Name), req)
	if es.Spec.Suspend {
		c.stop(k, true)
		return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhaseSuspended, false, false, ReasonSuspended, "suspended")
	}
	sinkRef, err := SinkRef(req.Workspace, es.Spec.Sink)
	if err != nil {
		c.stop(k, true)
		return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhaseFailed, false, true, ReasonInvalidSpec, err.Error())
	}
	sourceState := ""
	var rewind int64
	if src.Workspace == req.Workspace {
		var live eventsv1.EventSource
		err := req.Get(ctx, "", src.Name, &live)
		switch {
		case apierrors.IsNotFound(err):
			c.stop(k, true)
			return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhasePending, false, false, ReasonSourceMissing,
				"the EventSource "+src.Name+" does not exist")
		case err != nil:
			return ctlruntime.Result{}, err
		}
		bound := meta.FindStatusCondition(live.Status.Conditions, eventsv1.EventSourceConditionBackendBound)
		if bound == nil || bound.Status != metav1.ConditionTrue || bound.ObservedGeneration != live.Generation {
			c.stop(k, true)
			return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhasePending, false, false, ReasonSourceUnbound,
				"the EventSource "+src.Name+" is not bound to its backend")
		}
		sourceState = fmt.Sprintf("%s/%d/%s", live.UID, live.Generation, live.Spec.BackendRef.String())
		rewind = rewindFor(live.Spec.BackendRef.Kind)
	}
	fingerprint, err := fingerprintOf(es.Spec, src, sinkRef, sourceState)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	c.mu.Lock()
	current := c.streams[k]
	c.mu.Unlock()
	if current != nil && current.uid == string(es.UID) && current.fingerprint == fingerprint {
		current.update(req, es)
		return ctlruntime.Result{}, nil
	}
	c.stop(k, true)
	self := commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: "EventStream", Name: es.Name, Workspace: req.Workspace}
	filter := events.Filter{Collections: es.Spec.Filter.Collections, DIDs: es.Spec.Filter.DIDs}
	source, err := bus.Resolve[events.Source](ctx, c.bus, self, src, bus.Capability{Name: events.CapabilitySubscribe, Scope: filter.Scope()})
	if errors.Is(err, eventsource.ErrNotReconciled) {
		c.log.Info("an EventStream waits for its EventSource", "workspace", req.Workspace, "name", es.Name, "err", err.Error())
		return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhasePending, false, false, ReasonSourceNotReady,
			"the EventSource "+src.String()+" is not reconciled yet")
	}
	if err != nil {
		return ctlruntime.Result{}, errors.Join(err, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhaseFailed, false, true, ReasonResolveFailed,
			"resolving the EventSource "+src.String()+": "+err.Error()))
	}
	c.indexSink(k, key(sinkRef.Workspace, sinkRef.Name), req)
	sink, err := bus.Resolve[events.Sink](ctx, c.bus, self, sinkRef, bus.Capability{Name: events.CapabilityDeliver, Scope: []string{sinkRef.Name}})
	if errors.Is(err, atprotorecordwatch.ErrNotReconciled) {
		c.log.Info("an EventStream waits for its sink", "workspace", req.Workspace, "name", es.Name, "err", err.Error())
		return ctlruntime.Result{}, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhasePending, false, false, ReasonSinkNotReady,
			"the sink "+sinkRef.String()+" is not reconciled yet")
	}
	if err != nil {
		return ctlruntime.Result{}, errors.Join(err, c.applyStatus(ctx, req, es, eventsv1.EventStreamPhaseFailed, false, true, ReasonResolveFailed,
			"resolving the sink "+sinkRef.String()+": "+err.Error()))
	}
	s := c.start(ctx, k, req, es, fingerprint, filter, rewind, source, sink)
	s.flush.Mark()
	return ctlruntime.Result{}, s.publish(ctx)
}

func rewindFor(kind string) int64 {
	switch kind {
	case eventsv1.RelayEventSourceKind:
		return delivery.RelayRewindEvents
	case eventsv1.JetstreamEventSourceKind:
		return delivery.JetstreamRewindMicros
	}
	return 0
}

func fingerprintOf(spec eventsv1.EventStreamSpec, src, sink commonv1.ObjectRef, sourceState string) (string, error) {
	raw, err := json.Marshal(struct {
		Spec   eventsv1.EventStreamSpec
		Source commonv1.ObjectRef
		Sink   commonv1.ObjectRef
		State  string
	}{spec, src, sink, sourceState})
	if err != nil {
		return "", fmt.Errorf("eventstream: fingerprinting the spec: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Controller) stop(k string, flush bool) {
	c.mu.Lock()
	s := c.streams[k]
	delete(c.streams, k)
	c.mu.Unlock()
	if s != nil {
		s.stop(flush)
	}
}

func (c *Controller) applyStatus(ctx context.Context, req ctlruntime.Request, es *eventsv1.EventStream, phase eventsv1.EventStreamPhase,
	ready, failed bool, reason, message string) error {
	status := eventsv1.EventStreamStatus{ObservedGeneration: es.Generation, Phase: phase, Cursor: es.Status.Cursor, Lag: es.Status.Lag,
		LastEventAt: es.Status.LastEventAt, ServerFilter: es.Status.ServerFilter, Conditions: append([]metav1.Condition(nil), es.Status.Conditions...)}
	setConditions(&status, es.Generation, ready, false, failed, reason, message)
	next := es.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func setConditions(status *eventsv1.EventStreamStatus, generation int64, ready, lagging, failed bool, reason, message string) {
	readyReason := reason
	if ready {
		readyReason = delivery.ReasonStreamOpen
	}
	conds.Set(&status.Conditions, generation, eventsv1.EventStreamConditionReady, ready, readyReason, message)
	lagReason, lagMessage := "UpToDate", "the stream is current"
	if lagging {
		lagReason, lagMessage = "CursorGap", fmt.Sprintf("%d events behind", status.Lag)
	}
	conds.Set(&status.Conditions, generation, eventsv1.EventStreamConditionLagging, lagging, lagReason, lagMessage)
	failedReason, failedMessage := "NoError", "the stream is healthy"
	if failed {
		failedReason, failedMessage = reason, message
	}
	conds.Set(&status.Conditions, generation, eventsv1.EventStreamConditionFailed, failed, failedReason, failedMessage)
}

type stream struct {
	c *Controller

	key string

	uid string

	fingerprint string

	loop *delivery.Loop

	group *supervise.Group

	flush *debounce.Debounce

	progress *debounce.Debounce

	rewind int64

	mu sync.Mutex

	req ctlruntime.Request

	es *eventsv1.EventStream

	publishMu sync.Mutex

	last *eventsv1.EventStreamStatus
}

func (c *Controller) start(ctx context.Context, k string, req ctlruntime.Request, es *eventsv1.EventStream, fingerprint string,
	filter events.Filter, rewind int64, source events.Source, sink events.Sink) *stream {
	s := &stream{c: c, key: k, uid: string(es.UID), fingerprint: fingerprint, rewind: rewind, req: req, es: es.DeepCopy()}
	s.flush = debounce.New(c.flushDelay, s.flushNow)
	s.progress = debounce.New(delivery.ProgressDelay, s.flushNow)
	s.loop = delivery.New(delivery.Config{Filter: filter, Rewind: rewind, Live: es.Spec.Resume == eventsv1.EventStreamResumeLive},
		delivery.Deps{Source: source, Sink: sink, Resume: s.resume, Changed: s.flush.Mark, Progress: s.progress.Mark})
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopRoot := context.AfterFunc(c.root, cancel)
	s.group = supervise.New(streamCtx)
	s.group.Go("eventstream "+k, func(ctx context.Context) error {
		defer stopRoot()
		defer cancel()
		s.loop.Run(ctx)
		return nil
	})
	c.mu.Lock()
	c.streams[k] = s
	c.mu.Unlock()
	return s
}

func (s *stream) update(req ctlruntime.Request, es *eventsv1.EventStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.req, s.es = req, es.DeepCopy()
}

func (s *stream) stop(flush bool) {
	s.flush.Stop()
	s.progress.Stop()
	_ = s.group.Stop()
	if flush {
		s.flushNow()
	}
}

func (s *stream) resume() *int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.es.Spec.Resume == eventsv1.EventStreamResumeLive {
		return nil
	}
	if v, ok := s.c.cursors.Load(s.req.Workspace, s.es.Name, s.uid); ok {
		return delivery.Rewound(v, s.rewind)
	}
	if s.es.Status.Cursor > 0 {
		return delivery.Rewound(s.es.Status.Cursor, s.rewind)
	}
	if s.es.Spec.Cursor != nil {
		v := *s.es.Spec.Cursor
		return &v
	}
	return nil
}

func (s *stream) flushNow() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.c.root), statusTimeout)
	defer cancel()
	if err := s.publish(ctx); err != nil {
		s.c.log.Warn("writing the EventStream status", "stream", s.key, "err", err.Error())
	}
	state := s.loop.State()
	s.mu.Lock()
	workspace, name, live := s.req.Workspace, s.es.Name, s.es.Spec.Resume == eventsv1.EventStreamResumeLive
	s.mu.Unlock()
	if state.Started && !live {
		if err := s.c.cursors.Save(workspace, name, s.uid, state.Cursor); err != nil {
			s.c.log.Warn("saving the EventStream cursor", "stream", s.key, "err", err.Error())
		}
	}
}

func (s *stream) publish(ctx context.Context) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	state := s.loop.State()
	s.mu.Lock()
	req, es := s.req, s.es
	s.mu.Unlock()
	status := eventsv1.EventStreamStatus{ObservedGeneration: es.Generation, Phase: eventsv1.EventStreamPhase(state.Phase), Cursor: es.Status.Cursor,
		Lag: state.Lag, LastEventAt: es.Status.LastEventAt, ServerFilter: es.Status.ServerFilter, Conditions: append([]metav1.Condition(nil), es.Status.Conditions...)}
	if s.last != nil {
		status.Conditions = append([]metav1.Condition(nil), s.last.Conditions...)
		status.Cursor, status.LastEventAt, status.ServerFilter = s.last.Cursor, s.last.LastEventAt, s.last.ServerFilter
	}
	if f := state.ServerFilter; f != nil {
		status.ServerFilter = &eventsv1.EventStreamServerFilter{Collections: slices.Clone(f.Collections), DIDs: slices.Clone(f.DIDs),
			Supported: eventsv1.EventStreamServerFilterSupport(f.Supported)}
	}
	if state.Started {
		status.Cursor = state.Cursor
		at := metav1.NewTime(state.LastEventAt.UTC().Truncate(time.Second))
		status.LastEventAt = &at
	}
	setConditions(&status, es.Generation, state.Ready, state.Lag > 0, state.Failed, state.Reason, state.Message)
	if s.last != nil && statusEqual(*s.last, status) {
		return nil
	}
	next := es.DeepCopy()
	next.Status = status
	if err := req.ApplyStatus(ctx, next); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.last = &status
	return nil
}

func statusEqual(a, b eventsv1.EventStreamStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration || a.Phase != b.Phase || a.Cursor != b.Cursor || a.Lag != b.Lag {
		return false
	}
	if (a.LastEventAt == nil) != (b.LastEventAt == nil) || (a.LastEventAt != nil && !a.LastEventAt.Equal(b.LastEventAt)) {
		return false
	}
	if !reflect.DeepEqual(a.ServerFilter, b.ServerFilter) {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message || x.ObservedGeneration != y.ObservedGeneration {
			return false
		}
	}
	return true
}

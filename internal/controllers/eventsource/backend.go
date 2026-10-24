package eventsource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/delivery"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/debounce"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/wsconn"
)

const (
	Export = "events"

	ReasonConfigured = "Configured"

	ReasonStreaming = "Streaming"

	ReasonInvalidSpec = "InvalidSpec"

	ReasonUpstreamFailed = "UpstreamFailed"

	statusTimeout = 10 * time.Second
)

var ErrNotReady = errors.New("eventsource: the source is not reconciled yet")

var ErrNotReconciled = fmt.Errorf("%w", ErrNotReady)

type Upstream struct {
	Address string

	Source events.Source

	CountGaps bool
}

type Configure[T ctlruntime.Object] func(ctx context.Context, req ctlruntime.Request, obj T) (Upstream, error)

type Backend[T ctlruntime.Object] struct {
	root context.Context

	kind string

	bus *bus.Bus

	configure Configure[T]

	status func(T) *eventsv1.EventSourceStatus

	flushDelay time.Duration

	log *slog.Logger

	mu sync.Mutex

	entries map[string]*backendEntry[T]
}

type backendEntry[T ctlruntime.Object] struct {
	b *Backend[T]

	ref commonv1.ObjectRef

	tracker *delivery.Tracker

	flush *debounce.Debounce

	mu sync.Mutex

	publishMu sync.Mutex

	req ctlruntime.Request

	obj T

	upstream Upstream

	invalid error

	last *eventsv1.EventSourceStatus
}

type BackendOptions struct {
	Bus *bus.Bus

	Log *slog.Logger

	FlushDelay time.Duration
}

func NewBackend[T ctlruntime.Object](ctx context.Context, kind string, opts BackendOptions, configure Configure[T], status func(T) *eventsv1.EventSourceStatus) *Backend[T] {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	b := &Backend[T]{root: ctx, kind: kind, bus: opts.Bus, configure: configure, status: status, flushDelay: opts.FlushDelay,
		log: log.With("component", strings.ToLower(kind)+"-controller"), entries: map[string]*backendEntry[T]{}}
	bus.Provide[events.Source](opts.Bus, schema.GroupKind{Group: eventsv1.GroupName, Kind: kind}, events.CapabilitySubscribe, b.open)
	return b
}

func entryKey(workspace, name string) string {
	return strings.TrimSpace(workspace) + "/" + strings.TrimSpace(name)
}

func (b *Backend[T]) Reconcile(ctx context.Context, req ctlruntime.Request, obj T) (ctlruntime.Result, error) {
	key := entryKey(req.Workspace, obj.GetName())
	if !obj.GetDeletionTimestamp().IsZero() {
		b.mu.Lock()
		e := b.entries[key]
		delete(b.entries, key)
		b.mu.Unlock()
		if e != nil {
			e.flush.Stop()
		}
		return ctlruntime.Result{}, nil
	}
	b.mu.Lock()
	e := b.entries[key]
	created := e == nil
	if created {
		e = &backendEntry[T]{b: b, ref: commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: b.kind, Name: obj.GetName(), Workspace: req.Workspace}}
		e.flush = debounce.New(b.flushDelay, e.flushNow)
		b.entries[key] = e
	}
	b.mu.Unlock()
	upstream, err := b.configure(ctx, req, obj)
	e.mu.Lock()
	if e.tracker == nil || e.upstream.CountGaps != upstream.CountGaps || e.upstream.Address != upstream.Address {
		e.tracker = delivery.NewTracker(delivery.TrackerConfig{CountGaps: upstream.CountGaps, Seed: b.status(obj).Cursor})
	}
	recorded := created || e.obj.GetUID() != obj.GetUID() || e.obj.GetGeneration() != obj.GetGeneration()
	e.req, e.obj, e.upstream, e.invalid = req, obj, upstream, err
	e.mu.Unlock()
	if recorded {
		b.bus.NotifyBound(bus.Key{Workspace: req.Workspace, Kind: b.kind, Name: obj.GetName()})
	}
	if err != nil {
		b.log.Warn("the event source spec is invalid; it waits for a spec change", "workspace", req.Workspace, "name", obj.GetName(), "err", err.Error())
	}
	return ctlruntime.Result{}, e.publish(ctx)
}

func (e *backendEntry[T]) flushNow() {
	ctx, cancel := context.WithTimeout(e.b.root, statusTimeout)
	defer cancel()
	if err := e.publish(ctx); err != nil {
		e.b.log.Warn("writing the event source status", "workspace", e.ref.Workspace, "name", e.ref.Name, "err", err.Error())
	}
}

func (e *backendEntry[T]) publish(ctx context.Context) error {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	e.mu.Lock()
	req, obj, upstream, invalid, tracker := e.req, e.obj, e.upstream, e.invalid, e.tracker
	e.mu.Unlock()
	current := e.b.status(obj)
	generation := obj.GetGeneration()
	next := eventsv1.EventSourceStatus{ObservedGeneration: generation, Conditions: append([]metav1.Condition(nil), current.Conditions...)}
	snap := tracker.Snapshot()
	switch {
	case invalid != nil:
		conds.Set(&next.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonInvalidSpec, invalid.Error())
	case snap.Err != nil:
		next.Address = upstream.Address
		conds.Set(&next.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonUpstreamFailed, snap.Err.Error())
	case snap.Started:
		next.Address = upstream.Address
		conds.Set(&next.Conditions, generation, eventsv1.EventSourceConditionReady, true, ReasonStreaming, "the source delivers from "+upstream.Address)
	default:
		next.Address = upstream.Address
		conds.Set(&next.Conditions, generation, eventsv1.EventSourceConditionReady, true, ReasonConfigured, "the source subscribes to "+upstream.Address)
	}
	next.Cursor, next.Lag = snap.Cursor, snap.Lag
	if !snap.LastEventAt.IsZero() {
		at := metav1.NewTime(snap.LastEventAt.UTC().Truncate(time.Second))
		next.LastEventAt = &at
	} else {
		next.LastEventAt = current.LastEventAt
	}
	if e.last != nil && statusEqual(*e.last, next) {
		return nil
	}
	copied, ok := obj.DeepCopyObject().(T)
	if !ok {
		return fmt.Errorf("eventsource: %T does not copy to itself", obj)
	}
	*e.b.status(copied) = next
	if err := req.ApplyStatus(ctx, copied); err != nil {
		return err
	}
	e.last = &next
	return nil
}

func statusEqual(a, b eventsv1.EventSourceStatus) bool {
	strip := func(in []metav1.Condition) []metav1.Condition {
		out := make([]metav1.Condition, len(in))
		for i, c := range in {
			c.LastTransitionTime = metav1.Time{}
			out[i] = c
		}
		return out
	}
	a.Conditions, b.Conditions = strip(a.Conditions), strip(b.Conditions)
	return reflect.DeepEqual(a, b)
}

func (b *Backend[T]) open(ctx context.Context, to commonv1.ObjectRef, _ []string) (events.Source, error) {
	b.mu.Lock()
	e := b.entries[entryKey(to.Workspace, to.Name)]
	b.mu.Unlock()
	if e == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotReconciled, to.String())
	}
	e.mu.Lock()
	req, obj, invalid := e.req, e.obj, e.invalid
	e.mu.Unlock()
	if invalid != nil {
		return nil, fmt.Errorf("eventsource: %s: %w", to.String(), invalid)
	}
	live, ok := obj.DeepCopyObject().(T)
	if !ok {
		return nil, fmt.Errorf("eventsource: %T does not copy to itself", obj)
	}
	if err := req.Get(ctx, "", to.Name, live); err != nil {
		return nil, fmt.Errorf("eventsource: reading %s: %w", to.String(), err)
	}
	if live.GetUID() != obj.GetUID() || live.GetGeneration() != obj.GetGeneration() {
		return nil, fmt.Errorf("%w: %s changed since its last reconcile", ErrNotReconciled, to.String())
	}
	return handle[T]{e: e}, nil
}

type handle[T ctlruntime.Object] struct {
	e *backendEntry[T]
}

func (h handle[T]) Subscribe(ctx context.Context, filter events.Filter) (events.Stream, error) {
	h.e.mu.Lock()
	upstream, tracker := h.e.upstream, h.e.tracker
	h.e.mu.Unlock()
	ctx = bus.WithCaller(ctx, h.e.ref)
	stream, err := upstream.Source.Subscribe(ctx, filter)
	if err != nil {
		tracker.Fail(err)
		h.e.flush.Mark()
		return nil, err
	}
	tracker.Recover()
	h.e.flush.Mark()
	return delivery.NewTracked(stream, tracker, h.e.flush.Mark), nil
}

func BusDial(b *bus.Bus) wsconn.DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if b != nil {
			conn, err := b.DialHTTP(ctx, address)
			if err == nil {
				return conn, nil
			}
			if !errors.Is(err, bus.ErrNoHandler) {
				return nil, err
			}
		}
		return (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
}

func ReadyAtGeneration(conditions []metav1.Condition, generation int64) bool {
	c := meta.FindStatusCondition(conditions, eventsv1.EventSourceConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

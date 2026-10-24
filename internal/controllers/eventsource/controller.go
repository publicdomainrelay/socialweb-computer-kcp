package eventsource

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "eventsource"

	ReasonBound = "BackendBound"

	ReasonNotAContractKind = "NotAContractKind"

	ReasonCrossWorkspace = "CrossWorkspace"

	ReasonBackendMissing = "BackendMissing"

	ReasonBackendNotReady = "BackendNotReady"
)

type Options struct {
	Bus *bus.Bus

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	log *slog.Logger

	mu sync.Mutex

	sources map[string]sourceEntry

	byBackend map[string]map[string]bool
}

type sourceEntry struct {
	req ctlruntime.Request

	backend commonv1.ObjectRef
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{bus: opts.Bus, log: log.With("component", "eventsource-controller"), sources: map[string]sourceEntry{},
		byBackend: map[string]map[string]bool{}}
	bus.Provide[events.Source](opts.Bus, schema.GroupKind{Group: eventsv1.GroupName, Kind: eventsv1.EventSourceKind}, events.CapabilitySubscribe, c.open)
	err := ctlruntime.For[*eventsv1.EventSource](m, Name, c, ctlruntime.Export(Export),
		ctlruntime.Watches[*eventsv1.PdsEventSource](func(_ context.Context, src ctlruntime.Request, _ *eventsv1.PdsEventSource) []ctlruntime.Request {
			return c.sourcesOf(src, eventsv1.PdsEventSourceKind)
		}),
		ctlruntime.Watches[*eventsv1.RelayEventSource](func(_ context.Context, src ctlruntime.Request, _ *eventsv1.RelayEventSource) []ctlruntime.Request {
			return c.sourcesOf(src, eventsv1.RelayEventSourceKind)
		}),
		ctlruntime.Watches[*eventsv1.JetstreamEventSource](func(_ context.Context, src ctlruntime.Request, _ *eventsv1.JetstreamEventSource) []ctlruntime.Request {
			return c.sourcesOf(src, eventsv1.JetstreamEventSourceKind)
		}))
	if err != nil {
		return nil, fmt.Errorf("eventsource: registering the EventSource controller: %w", err)
	}
	return c, nil
}

func backendKey(workspace, kind, name string) string {
	return strings.TrimSpace(workspace) + "/" + strings.ToLower(strings.TrimSpace(kind)) + "/" + strings.TrimSpace(name)
}

func (c *Controller) sourcesOf(src ctlruntime.Request, kind string) []ctlruntime.Request {
	c.mu.Lock()
	names := c.byBackend[backendKey(src.Workspace, kind, src.Name)]
	out := make([]ctlruntime.Request, 0, len(names)+1)
	for name := range names {
		out = append(out, ctlruntime.Request{Cluster: src.Cluster, Namespace: src.Namespace, Name: name})
	}
	c.mu.Unlock()
	if !slices.ContainsFunc(out, func(r ctlruntime.Request) bool { return r.Name == src.Name }) {
		out = append(out, ctlruntime.Request{Cluster: src.Cluster, Namespace: src.Namespace, Name: src.Name})
	}
	return out
}

func (c *Controller) index(key string, backend commonv1.ObjectRef, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.sources[key]; ok {
		bk := backendKey(prior.backend.Workspace, prior.backend.Kind, prior.backend.Name)
		delete(c.byBackend[bk], name)
		if len(c.byBackend[bk]) == 0 {
			delete(c.byBackend, bk)
		}
	}
	if backend.Kind == "" {
		delete(c.sources, key)
		return
	}
	bk := backendKey(backend.Workspace, backend.Kind, backend.Name)
	if c.byBackend[bk] == nil {
		c.byBackend[bk] = map[string]bool{}
	}
	c.byBackend[bk][name] = true
}

func IsBackendKind(ref commonv1.ObjectRef) bool {
	if g := strings.TrimSpace(ref.Group); g != "" && g != eventsv1.GroupName {
		return false
	}
	for _, r := range eventsv1.APIResources {
		if strings.EqualFold(r.Kind, strings.TrimSpace(ref.Kind)) && slices.Contains(r.Contracts, commonv1.ContractEventSources) {
			return true
		}
	}
	return false
}

func backendObject(kind string) (ctlruntime.Object, func() (*eventsv1.EventSourceStatus, int64)) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case strings.ToLower(eventsv1.PdsEventSourceKind):
		o := &eventsv1.PdsEventSource{}
		return o, func() (*eventsv1.EventSourceStatus, int64) { return &o.Status, o.Generation }
	case strings.ToLower(eventsv1.RelayEventSourceKind):
		o := &eventsv1.RelayEventSource{}
		return o, func() (*eventsv1.EventSourceStatus, int64) { return &o.Status, o.Generation }
	case strings.ToLower(eventsv1.JetstreamEventSourceKind):
		o := &eventsv1.JetstreamEventSource{}
		return o, func() (*eventsv1.EventSourceStatus, int64) { return &o.Status, o.Generation }
	}
	return nil, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, es *eventsv1.EventSource) (ctlruntime.Result, error) {
	key := entryKey(req.Workspace, es.Name)
	if !es.DeletionTimestamp.IsZero() {
		c.index(key, commonv1.ObjectRef{}, es.Name)
		return ctlruntime.Result{}, nil
	}
	ref := es.Spec.BackendRef
	ref.Group = cmp.Or(strings.TrimSpace(ref.Group), eventsv1.GroupName)
	ref.Workspace = ref.InWorkspace(req.Workspace)
	c.index(key, ref, es.Name)
	c.mu.Lock()
	prior, held := c.sources[key]
	c.sources[key] = sourceEntry{req: req, backend: ref}
	c.mu.Unlock()
	if !held || prior.backend != ref {
		c.bus.NotifyBound(bus.Key{Workspace: req.Workspace, Kind: eventsv1.EventSourceKind, Name: es.Name})
	}
	generation := es.Generation
	status := eventsv1.EventSourceStatus{ObservedGeneration: generation, Conditions: append([]metav1.Condition(nil), es.Status.Conditions...)}
	bound := func(ok bool, reason, message string) {
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionBackendBound, ok, reason, message)
	}
	ready := func(ok bool, reason, message string) {
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, ok, reason, message)
	}
	obj, backendStatus := backendObject(ref.Kind)
	switch {
	case !IsBackendKind(ref) || obj == nil:
		bound(false, ReasonNotAContractKind, fmt.Sprintf("%s is not a kind of the eventsources contract", ref.String()))
		ready(false, ReasonNotAContractKind, "the backend is not bound")
	case ref.Workspace != req.Workspace:
		bound(false, ReasonCrossWorkspace, fmt.Sprintf("the backend %s is not in the workspace of the EventSource", ref.String()))
		ready(false, ReasonCrossWorkspace, "the backend is not bound")
	default:
		err := req.Get(ctx, "", ref.Name, obj)
		switch {
		case apierrors.IsNotFound(err):
			bound(false, ReasonBackendMissing, fmt.Sprintf("the backend %s does not exist", ref.String()))
			ready(false, ReasonBackendMissing, "the backend is not bound")
		case err != nil:
			return ctlruntime.Result{}, fmt.Errorf("eventsource: reading %s: %w", ref.String(), err)
		default:
			s, backendGeneration := backendStatus()
			bound(true, ReasonBound, fmt.Sprintf("bound to %s", ref.String()))
			status.Cursor, status.LastEventAt, status.Lag, status.Address = s.Cursor, s.LastEventAt, s.Lag, s.Address
			if r := meta.FindStatusCondition(s.Conditions, eventsv1.EventSourceConditionReady); ReadyAtGeneration(s.Conditions, backendGeneration) {
				ready(true, r.Reason, r.Message)
			} else if r != nil {
				ready(false, cmp.Or(r.Reason, ReasonBackendNotReady), r.Message)
			} else {
				ready(false, ReasonBackendNotReady, "the backend reports no Ready condition")
			}
		}
	}
	next := es.DeepCopy()
	next.Status = status
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func (c *Controller) open(ctx context.Context, to commonv1.ObjectRef, scope []string) (events.Source, error) {
	key := entryKey(to.Workspace, to.Name)
	c.mu.Lock()
	entry, ok := c.sources[key]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotReconciled, to.String())
	}
	var live eventsv1.EventSource
	if err := entry.req.Get(ctx, "", to.Name, &live); err != nil {
		return nil, fmt.Errorf("eventsource: reading %s: %w", to.String(), err)
	}
	if b := meta.FindStatusCondition(live.Status.Conditions, eventsv1.EventSourceConditionBackendBound); b == nil || b.Status != metav1.ConditionTrue ||
		b.ObservedGeneration != live.Generation {
		return nil, fmt.Errorf("%w: %s is not bound to its backend", ErrNotReady, to.String())
	}
	self := commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: eventsv1.EventSourceKind, Name: live.Name, Workspace: to.Workspace}
	return bus.Resolve[events.Source](ctx, c.bus, self, entry.backend, bus.Capability{Name: events.CapabilitySubscribe, Scope: scope})
}

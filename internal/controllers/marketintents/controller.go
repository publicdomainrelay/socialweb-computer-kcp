package marketintents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	Name = "marketintents"

	ExportComputeVMs = "computevms"

	ExportApplications = "applications"

	ExportComputeContracts = "computecontracts"

	ExportExposures = "exposures"

	ExportMarketIdentities = "marketidentities"
)

type Options struct {
	Bus *bus.Bus

	Clients func(workspace string) (client.Client, error)

	Scope func(workspace string) bool

	Log *slog.Logger
}

type key struct {
	workspace string

	kind string

	name string
}

type subscriber struct {
	workspaces map[string]bool

	kinds map[string]bool

	observer market.Observer

	synced func()

	pending map[key]bool

	fired bool

	ctx context.Context
}

type Controller struct {
	clients func(workspace string) (client.Client, error)

	scope func(workspace string) bool

	log *slog.Logger

	mu sync.Mutex

	clusters map[key]string

	subs map[*subscriber]struct{}

	events map[string]*ctlruntime.Queue

	owners map[string]ownerUID

	locksMu sync.Mutex

	statusLocks map[string]*statusLock
}

type statusLock struct {
	mu sync.Mutex

	users int
}

func (c *Controller) lockStatus(key string) func() {
	c.locksMu.Lock()
	if c.statusLocks == nil {
		c.statusLocks = map[string]*statusLock{}
	}
	lock := c.statusLocks[key]
	if lock == nil {
		lock = &statusLock{}
		c.statusLocks[key] = lock
	}
	lock.users++
	c.locksMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		c.locksMu.Lock()
		if lock.users--; lock.users == 0 {
			delete(c.statusLocks, key)
		}
		c.locksMu.Unlock()
	}
}

type ownerUID struct {
	uid string

	at time.Time
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Bus == nil {
		return nil, errors.New("marketintents: the bus is required")
	}
	if opts.Clients == nil {
		return nil, errors.New("marketintents: the workspace clients are required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{clients: opts.Clients, scope: opts.Scope, log: log.With("component", "marketintents"), clusters: map[key]string{},
		subs: map[*subscriber]struct{}{}, events: map[string]*ctlruntime.Queue{}, owners: map[string]ownerUID{}}
	err := errors.Join(
		forward(m, c, opts.Scope, market.KindBid, ExportComputeVMs, "", 4, func(ch *market.Change, o *v1alpha1.Bid) { ch.Bid = o }),
		forward(m, c, opts.Scope, market.KindComputeVM, ExportComputeVMs, "", 2, func(ch *market.Change, o *v1alpha1.ComputeVM) { ch.ComputeVM = o }),
		forward(m, c, opts.Scope, market.KindStateSync, ExportApplications, "", 4, func(ch *market.Change, o *v1alpha1.StateSync) { ch.StateSync = o }),
		forward(m, c, opts.Scope, market.KindComputeContract, ExportComputeContracts, v1alpha1.FinalizerComputeContract, 4,
			func(ch *market.Change, o *v1alpha1.ComputeContract) { ch.ComputeContract = o }),
		forward(m, c, opts.Scope, market.KindExposure, ExportExposures, "", 1, func(ch *market.Change, o *exposurev1alpha1.Exposure) { ch.Exposure = o }),
		forward(m, c, opts.Scope, market.KindMarketIdentity, ExportMarketIdentities, "", 1,
			func(ch *market.Change, o *v1alpha1.MarketIdentity) { ch.MarketIdentity = o }),
	)
	if err != nil {
		return nil, err
	}
	for _, kind := range []string{"Bidder", "Requester"} {
		bus.Provide[market.Intents](opts.Bus, schema.GroupKind{Group: v1alpha1.GroupName, Kind: kind}, market.CapabilityIntents, c.open)
	}
	return c, nil
}

type object[T any] interface {
	ctlruntime.Object
	DeepCopy() T
}

type forwarder[T object[T]] struct {
	c *Controller

	kind string

	finalizer string

	fill func(*market.Change, T)
}

func forward[T object[T]](m *ctlruntime.Manager, c *Controller, scope func(string) bool, kind, export, finalizer string, workers int,
	fill func(*market.Change, T)) error {
	queue := ctlruntime.NewQueue()
	events, err := queue.Run(m, Name+"-"+strings.ToLower(kind)+": replay")
	if err != nil {
		return fmt.Errorf("marketintents: the %s replay: %w", kind, err)
	}
	c.mu.Lock()
	c.events[kind] = queue
	c.mu.Unlock()
	f := &forwarder[T]{c: c, kind: kind, finalizer: finalizer, fill: fill}
	opts := []ctlruntime.Option{ctlruntime.Export(export), ctlruntime.Events(events), ctlruntime.Gone(f.gone), ctlruntime.Workers(workers),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj T) []ctlruntime.Request {
			return []ctlruntime.Request{{Cluster: src.Cluster, Namespace: obj.GetNamespace(), Name: obj.GetName()}}
		})}
	if finalizer != "" {
		opts = append(opts, ctlruntime.Finalizer(finalizer))
	}
	if scope != nil {
		opts = append(opts, ctlruntime.InWorkspaces(scope))
	}
	if err := ctlruntime.For[T](m, Name+"-"+strings.ToLower(kind), f, opts...); err != nil {
		return fmt.Errorf("marketintents: registering the %s forwarder: %w", kind, err)
	}
	return nil
}

func (f *forwarder[T]) Reconcile(ctx context.Context, req ctlruntime.Request, obj T) (ctlruntime.Result, error) {
	k := key{workspace: req.Workspace, kind: f.kind, name: req.Name}
	f.c.mu.Lock()
	f.c.clusters[k] = req.Cluster
	f.c.mu.Unlock()
	change := market.Change{Workspace: req.Workspace, Kind: f.kind, Name: req.Name}
	f.fill(&change, obj.DeepCopy())
	wait, claimed, err := f.c.deliver(ctx, change)
	waiting := f.finalizer != "" && !obj.GetDeletionTimestamp().IsZero() && (wait > 0 || (claimed == 0 && holdsGuest(obj)))
	return ctlruntime.Result{RequeueAfter: wait, Waiting: waiting}, err
}

func holdsGuest(obj any) bool {
	if h, ok := obj.(interface{ HoldsGuest() bool }); ok {
		return h.HoldsGuest()
	}
	return true
}

func (f *forwarder[T]) gone(ctx context.Context, req ctlruntime.Request) {
	k := key{workspace: req.Workspace, kind: f.kind, name: req.Name}
	f.c.mu.Lock()
	delete(f.c.clusters, k)
	f.c.mu.Unlock()
	if _, _, err := f.c.deliver(ctx, market.Change{Workspace: req.Workspace, Kind: f.kind, Name: req.Name, Gone: true}); err != nil {
		f.c.log.Warn("delivering a deletion failed", "kind", f.kind, "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
}

func (c *Controller) deliver(ctx context.Context, change market.Change) (time.Duration, int, error) {
	k := key{workspace: change.Workspace, kind: change.Kind, name: change.Name}
	c.mu.Lock()
	var targets []*subscriber
	for s := range c.subs {
		if s.matches(change.Workspace, change.Kind) {
			targets = append(targets, s)
		}
	}
	c.mu.Unlock()
	var wait time.Duration
	var claimed int
	var errs []error
	for _, s := range targets {
		if s.kinds[change.Kind] {
			claimed++
		}
		d, err := s.observer.Observe(ctx, change)
		if err != nil {
			errs = append(errs, err)
		}
		if d > 0 && (wait == 0 || d < wait) {
			wait = d
		}
		c.observed(s, k)
	}
	return wait, claimed, errors.Join(errs...)
}

func (c *Controller) subscribe(ctx context.Context, home string, sub market.Subscription) (func(), error) {
	if sub.Observer == nil {
		return nil, errors.New("marketintents: a subscription needs an observer")
	}
	s := &subscriber{workspaces: map[string]bool{home: true}, kinds: map[string]bool{}, observer: sub.Observer, synced: sub.Synced, ctx: ctx}
	for _, ws := range sub.Workspaces {
		if ws = strings.TrimSpace(ws); ws != "" {
			s.workspaces[ws] = true
		}
	}
	for _, kind := range sub.Kinds {
		if kind = strings.TrimSpace(kind); kind != "" {
			s.kinds[kind] = true
		}
	}
	var listed map[key]string
	if s.synced != nil {
		var err error
		if listed, err = c.listed(ctx, s); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	c.subs[s] = struct{}{}
	replay := c.replayLocked(s)
	if s.synced != nil {
		s.pending = map[key]bool{}
		for k, annotated := range listed {
			if _, known := c.clusters[k]; known {
				s.pending[k] = true
				continue
			}
			if cluster := c.clusterOfLocked(k, annotated); cluster != "" {
				s.pending[k] = true
				replay[k.kind] = append(replay[k.kind], ctlruntime.Request{Cluster: cluster, Name: k.name})
			}
		}
	}
	c.mu.Unlock()
	c.enqueue(replay)
	c.settle(s)
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs, s)
			c.mu.Unlock()
		})
	}
	context.AfterFunc(ctx, cancel)
	return cancel, nil
}

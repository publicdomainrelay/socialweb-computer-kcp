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

	eventBuffer = 1024
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
}

type Controller struct {
	clients func(workspace string) (client.Client, error)

	log *slog.Logger

	mu sync.Mutex

	clusters map[key]string

	subs map[*subscriber]struct{}

	events map[string]chan ctlruntime.Request

	owners map[string]ownerUID
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
	c := &Controller{clients: opts.Clients, log: log.With("component", "marketintents"), clusters: map[key]string{},
		subs: map[*subscriber]struct{}{}, events: map[string]chan ctlruntime.Request{}, owners: map[string]ownerUID{}}
	err := errors.Join(
		forward(m, c, opts.Scope, market.KindBid, ExportComputeVMs, 4, func(ch *market.Change, o *v1alpha1.Bid) { ch.Bid = o }),
		forward(m, c, opts.Scope, market.KindComputeVM, ExportComputeVMs, 2, func(ch *market.Change, o *v1alpha1.ComputeVM) { ch.ComputeVM = o }),
		forward(m, c, opts.Scope, market.KindStateSync, ExportApplications, 4, func(ch *market.Change, o *v1alpha1.StateSync) { ch.StateSync = o }),
		forward(m, c, opts.Scope, market.KindComputeContract, ExportComputeContracts, 4,
			func(ch *market.Change, o *v1alpha1.ComputeContract) { ch.ComputeContract = o }),
		forward(m, c, opts.Scope, market.KindExposure, ExportExposures, 1, func(ch *market.Change, o *exposurev1alpha1.Exposure) { ch.Exposure = o }),
		forward(m, c, opts.Scope, market.KindMarketIdentity, ExportMarketIdentities, 1,
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

	fill func(*market.Change, T)
}

func forward[T object[T]](m *ctlruntime.Manager, c *Controller, scope func(string) bool, kind, export string, workers int,
	fill func(*market.Change, T)) error {
	events := make(chan ctlruntime.Request, eventBuffer)
	c.mu.Lock()
	c.events[kind] = events
	c.mu.Unlock()
	f := &forwarder[T]{c: c, kind: kind, fill: fill}
	opts := []ctlruntime.Option{ctlruntime.Export(export), ctlruntime.Events(events), ctlruntime.Gone(f.gone), ctlruntime.Workers(workers),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj T) []ctlruntime.Request {
			return []ctlruntime.Request{{Cluster: src.Cluster, Namespace: obj.GetNamespace(), Name: obj.GetName()}}
		})}
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
	wait, err := f.c.deliver(ctx, change)
	return ctlruntime.Result{RequeueAfter: wait}, err
}

func (f *forwarder[T]) gone(ctx context.Context, req ctlruntime.Request) {
	k := key{workspace: req.Workspace, kind: f.kind, name: req.Name}
	f.c.mu.Lock()
	delete(f.c.clusters, k)
	f.c.mu.Unlock()
	if _, err := f.c.deliver(ctx, market.Change{Workspace: req.Workspace, Kind: f.kind, Name: req.Name, Gone: true}); err != nil {
		f.c.log.Warn("delivering a deletion failed", "kind", f.kind, "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
}

func (c *Controller) deliver(ctx context.Context, change market.Change) (time.Duration, error) {
	c.mu.Lock()
	var targets []market.Observer
	for s := range c.subs {
		if s.workspaces[change.Workspace] && (len(s.kinds) == 0 || s.kinds[change.Kind]) {
			targets = append(targets, s.observer)
		}
	}
	c.mu.Unlock()
	var wait time.Duration
	var errs []error
	for _, observer := range targets {
		d, err := observer.Observe(ctx, change)
		if err != nil {
			errs = append(errs, err)
		}
		if d > 0 && (wait == 0 || d < wait) {
			wait = d
		}
	}
	return wait, errors.Join(errs...)
}

func (c *Controller) subscribe(ctx context.Context, home string, sub market.Subscription) (func(), error) {
	if sub.Observer == nil {
		return nil, errors.New("marketintents: a subscription needs an observer")
	}
	s := &subscriber{workspaces: map[string]bool{home: true}, kinds: map[string]bool{}, observer: sub.Observer}
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
	c.mu.Lock()
	c.subs[s] = struct{}{}
	replay := map[string][]ctlruntime.Request{}
	for k, cluster := range c.clusters {
		if s.workspaces[k.workspace] && (len(s.kinds) == 0 || s.kinds[k.kind]) {
			replay[k.kind] = append(replay[k.kind], ctlruntime.Request{Cluster: cluster, Name: k.name})
		}
	}
	events := map[string]chan ctlruntime.Request{}
	for kind := range replay {
		events[kind] = c.events[kind]
	}
	c.mu.Unlock()
	go func() {
		for kind, reqs := range replay {
			for _, r := range reqs {
				select {
				case events[kind] <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
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

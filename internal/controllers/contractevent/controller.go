package contractevent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	Name = "contractevent"

	Export = "computecontracts"
)

type Options struct {
	Bus *bus.Bus

	Clients func(workspace string) (client.Client, error)

	Scope func(workspace string) bool

	Log *slog.Logger
}

type eventKey struct {
	workspace string

	name string
}

type contractKey struct {
	cluster string

	name string
}

type subscriber struct {
	observer market.ContractEventObserver
}

type Controller struct {
	clients func(workspace string) (client.Client, error)

	log *slog.Logger

	queue *ctlruntime.Queue

	mu sync.Mutex

	known map[eventKey]contractKey

	byContract map[contractKey]map[string]bool

	subs map[string]map[*subscriber]struct{}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Bus == nil {
		return nil, errors.New("contractevent: the bus is required")
	}
	if opts.Clients == nil {
		return nil, errors.New("contractevent: the workspace clients are required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{clients: opts.Clients, log: log.With("component", Name), queue: ctlruntime.NewQueue(),
		known: map[eventKey]contractKey{}, byContract: map[contractKey]map[string]bool{},
		subs: map[string]map[*subscriber]struct{}{}}
	events, err := c.queue.Run(m, Name+": replay")
	if err != nil {
		return nil, fmt.Errorf("contractevent: the replay source: %w", err)
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(events), ctlruntime.Gone(c.gone), ctlruntime.Workers(2),
		ctlruntime.FieldManager(ctlruntime.FieldManagerPrefix + "requester"),
		ctlruntime.Watches(c.contractChanged)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.ContractEvent](m, Name, c, options...); err != nil {
		return nil, fmt.Errorf("contractevent: registering the controller: %w", err)
	}
	bus.Provide[market.ContractEvents](opts.Bus, schema.GroupKind{Group: v1alpha1.GroupName, Kind: "Requester"}, market.CapabilityContractEvents, c.open)
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.ContractEvent) (ctlruntime.Result, error) {
	contractName := strings.TrimSpace(obj.Spec.ContractRef.Name)
	c.index(req, contractName)
	change := market.ContractEventChange{Workspace: req.Workspace, Name: req.Name, Event: obj.DeepCopy()}
	if contractName != "" {
		contract := &v1alpha1.ComputeContract{}
		switch err := req.Get(ctx, "", contractName, contract); {
		case err == nil:
			change.Contract = contract
		case apierrors.IsNotFound(err):
		default:
			return ctlruntime.Result{}, fmt.Errorf("contractevent: reading the ComputeContract %s: %w", contractName, err)
		}
	}
	wait, err := c.deliver(ctx, change)
	return ctlruntime.Result{RequeueAfter: wait}, err
}

func (c *Controller) index(req ctlruntime.Request, contractName string) {
	k := eventKey{workspace: req.Workspace, name: req.Name}
	next := contractKey{cluster: req.Cluster, name: contractName}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.known[k]; ok && old != next {
		c.unindexLocked(old, req.Name)
	}
	c.known[k] = next
	if contractName == "" {
		return
	}
	names := c.byContract[next]
	if names == nil {
		names = map[string]bool{}
		c.byContract[next] = names
	}
	names[req.Name] = true
}

func (c *Controller) unindexLocked(k contractKey, name string) {
	names := c.byContract[k]
	delete(names, name)
	if len(names) == 0 {
		delete(c.byContract, k)
	}
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	k := eventKey{workspace: req.Workspace, name: req.Name}
	c.mu.Lock()
	if old, ok := c.known[k]; ok {
		c.unindexLocked(old, req.Name)
		delete(c.known, k)
	}
	c.mu.Unlock()
	if _, err := c.deliver(ctx, market.ContractEventChange{Workspace: req.Workspace, Name: req.Name, Gone: true}); err != nil {
		c.log.Warn("delivering a ContractEvent deletion failed", "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
}

func (c *Controller) contractChanged(_ context.Context, src ctlruntime.Request, obj *v1alpha1.ComputeContract) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := c.byContract[contractKey{cluster: src.Cluster, name: obj.GetName()}]
	out := make([]ctlruntime.Request, 0, len(names))
	for name := range names {
		out = append(out, ctlruntime.Request{Cluster: src.Cluster, Name: name})
	}
	return out
}

func (c *Controller) deliver(ctx context.Context, change market.ContractEventChange) (time.Duration, error) {
	c.mu.Lock()
	targets := make([]*subscriber, 0, len(c.subs[change.Workspace]))
	for s := range c.subs[change.Workspace] {
		targets = append(targets, s)
	}
	c.mu.Unlock()
	var wait time.Duration
	var errs []error
	for _, s := range targets {
		d, err := s.observer.ObserveContractEvent(ctx, change)
		if err != nil {
			errs = append(errs, err)
		}
		if d > 0 && (wait == 0 || d < wait) {
			wait = d
		}
	}
	return wait, errors.Join(errs...)
}

func (c *Controller) subscribe(ctx context.Context, workspace string, observer market.ContractEventObserver) (func(), error) {
	if observer == nil {
		return nil, errors.New("contractevent: a subscription needs an observer")
	}
	s := &subscriber{observer: observer}
	c.mu.Lock()
	if c.subs[workspace] == nil {
		c.subs[workspace] = map[*subscriber]struct{}{}
	}
	c.subs[workspace][s] = struct{}{}
	replay := []ctlruntime.Request{}
	for k, at := range c.known {
		if k.workspace == workspace {
			replay = append(replay, ctlruntime.Request{Cluster: at.cluster, Name: k.name})
		}
	}
	c.mu.Unlock()
	c.queue.Add(replay...)
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs[workspace], s)
			if len(c.subs[workspace]) == 0 {
				delete(c.subs, workspace)
			}
			c.mu.Unlock()
		})
	}
	context.AfterFunc(ctx, cancel)
	return cancel, nil
}

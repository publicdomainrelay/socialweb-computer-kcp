package localrepo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	atrepo "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/scoped"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	pdsctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/pds"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "localrepo"

	Export = "applications"

	Kind = "LocalRepo"

	PdsKind = "Pds"

	ReasonReady = "Ready"

	ReasonInvalidSpec = "InvalidSpec"

	ReasonLinkDenied = "LinkDenied"

	ReasonPdsUnavailable = "PdsUnavailable"

	ReasonAccountMissing = "AccountMissing"

	ReasonAccountInactive = "AccountInactive"

	ReasonObserveFailed = "ObserveFailed"

	commitQueue = 256
)

var GroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind}

type Options struct {
	Bus *bus.Bus

	Network atprotorepo.RecordReader

	Pds pdsctl.Lookup

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	network atprotorepo.RecordReader

	pds pdsctl.Lookup

	log *slog.Logger

	backends *binding.Table[backend]

	commits chan ctlruntime.Request

	mu sync.Mutex

	feeds map[string]*commitFeed

	ready map[binding.Target]bool

	closed bool

	relays sync.WaitGroup
}

type commitFeed struct {
	srv *atpds.Server

	stop chan struct{}
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, network: opts.Network, pds: opts.Pds, log: log.With("component", "localrepo-controller"),
		backends: binding.New[backend](), commits: make(chan ctlruntime.Request, commitQueue), feeds: map[string]*commitFeed{},
		ready: map[binding.Target]bool{}}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	ctl.Provide()
	options := []ctlruntime.Option{ctlruntime.Export(Export),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Pds) []ctlruntime.Request {
			var out []ctlruntime.Request
			for _, t := range ctl.backends.Users(binding.Key(src.Workspace, PdsKind, obj.Name)) {
				out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
			}
			return out
		}),
		ctlruntime.Events(ctl.commits),
		ctlruntime.Gone(func(_ context.Context, req ctlruntime.Request) {
			ctl.release(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name})
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.LocalRepo](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("localrepo: registering the LocalRepo controller: %w", err)
	}
	if err := m.Go(Name+": commit feeds", ctl.serveFeeds); err != nil {
		return nil, fmt.Errorf("localrepo: the commit feeds: %w", err)
	}
	return ctl, nil
}

func (c *Controller) serveFeeds(ctx context.Context) error {
	<-ctx.Done()
	c.mu.Lock()
	c.closed = true
	for key, feed := range c.feeds {
		close(feed.stop)
		delete(c.feeds, key)
	}
	c.mu.Unlock()
	c.relays.Wait()
	return nil
}

func (c *Controller) Provide() {
	provide[atprotorepo.Repo](c, atprotorepo.CapabilityWrite)
	provide[atprotorepo.RecordWriter](c, atprotorepo.CapabilityWrite)
	provide[atprotorepo.RecordReader](c, atprotorepo.CapabilityRead)
	provide[atprotorepo.Subscriber](c, atprotorepo.CapabilitySubscribe)
}

func provide[T any](c *Controller, capability string) {
	bus.Provide(c.bus, GroupKind, capability, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (T, error) {
		var zero T
		b, ok := c.backends.Get(to.Workspace, to.Name)
		if !ok {
			return zero, fmt.Errorf("%w: the LocalRepo %s is not reconciled", atprotorepo.ErrNotBound, to.String())
		}
		repo, err := open(ctx, c.bus, b, scope, c.network)
		if err != nil {
			return zero, err
		}
		out, ok := repo.(T)
		if !ok {
			return zero, fmt.Errorf("localrepo: the repo handle is not a %T", zero)
		}
		return out, nil
	})
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.LocalRepo) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.release(target)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.AtprotoRepoBackendStatus{ObservedGeneration: generation, DID: strings.TrimSpace(object.Spec.DID),
		Conditions: slices.Clone(object.Status.Conditions)}
	pdsRef := object.Spec.PdsRef
	pdsRef.Group = v1alpha1.GroupName
	pdsRef.Workspace = pdsRef.InWorkspace(req.Workspace)
	switch {
	case !strings.EqualFold(strings.TrimSpace(pdsRef.Kind), PdsKind) || strings.TrimSpace(pdsRef.Name) == "":
		c.release(target)
		return c.notReady(ctx, req, object, status, ReasonInvalidSpec, "the pdsRef must name a Pds component", nil)
	case !validDID(status.DID):
		c.release(target)
		return c.notReady(ctx, req, object, status, ReasonInvalidSpec, fmt.Sprintf("the did %q is not a DID", status.DID), nil)
	}
	repo := backend{Self: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: object.Name, Workspace: req.Workspace},
		Labels: maps.Clone(object.Labels), Pds: pdsRef, DID: status.DID}
	pdsKey := binding.Key(pdsRef.Workspace, PdsKind, pdsRef.Name)
	prior, held := c.backends.Get(req.Workspace, req.Name)
	c.backends.Set(target, &repo, pdsKey)
	if !held || prior.Self != repo.Self || prior.Pds != repo.Pds || prior.DID != repo.DID || !maps.Equal(prior.Labels, repo.Labels) {
		c.bus.NotifyBound(bus.Key{Workspace: req.Workspace, Kind: Kind, Name: req.Name})
	}
	c.follow(pdsKey, pdsRef)
	c.prune()
	handle, err := accounts(ctx, c.bus, repo)
	if err != nil {
		return c.resolveFailed(ctx, req, object, status, err)
	}
	info, err := handle.Describe(ctx)
	if err != nil {
		return c.notReady(ctx, req, object, status, ReasonObserveFailed, err.Error(), err)
	}
	status.Address = info.URL
	account, err := handle.Account(ctx, status.DID)
	if errors.Is(err, pds.ErrNotFound) {
		return c.notReady(ctx, req, object, status, ReasonAccountMissing, err.Error(), nil)
	}
	if err != nil {
		return c.notReady(ctx, req, object, status, ReasonObserveFailed, err.Error(), err)
	}
	status.KeyFingerprint = account.SigningKey
	commits, err := syncer(ctx, c.bus, repo)
	if err != nil {
		return c.resolveFailed(ctx, req, object, status, err)
	}
	if head, err := commits.LatestCommit(ctx, status.DID); err == nil {
		status.Head, status.Rev = head.CID, head.Rev
	} else if !errors.Is(err, pds.ErrNotFound) {
		return c.notReady(ctx, req, object, status, ReasonObserveFailed, err.Error(), err)
	}
	if _, err := open(ctx, c.bus, repo, nil, nil); err != nil {
		return c.resolveFailed(ctx, req, object, status, err)
	}
	if !account.Active {
		return c.notReady(ctx, req, object, status, ReasonAccountInactive, "the Pds account "+status.DID+" is not active", nil)
	}
	status.Writable = account.SigningKey != ""
	conds.Set(&status.Conditions, generation, commonv1.ConditionReady, true, ReasonReady,
		fmt.Sprintf("the Pds %s signs the repo %s in process", pdsRef.Name, status.DID))
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	c.settle(req, true)
	return ctlruntime.Result{}, nil
}

func (c *Controller) settle(req ctlruntime.Request, ready bool) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	c.mu.Lock()
	was := c.ready[target]
	if ready {
		c.ready[target] = true
	} else {
		delete(c.ready, target)
	}
	c.mu.Unlock()
	if ready && !was {
		c.bus.NotifyBound(bus.Key{Workspace: req.Workspace, Kind: Kind, Name: req.Name})
	}
}

func (c *Controller) resolveFailed(ctx context.Context, req ctlruntime.Request, object *v1alpha1.LocalRepo,
	status v1alpha1.AtprotoRepoBackendStatus, err error) (ctlruntime.Result, error) {
	switch {
	case errors.Is(err, bus.ErrLinkDenied):
		return c.notReady(ctx, req, object, status, ReasonLinkDenied, err.Error(), err)
	case errors.Is(err, pds.ErrNotFound), errors.Is(err, atprotorepo.ErrNotFound), errors.Is(err, bus.ErrNoProvider):
		return c.notReady(ctx, req, object, status, ReasonPdsUnavailable, err.Error(), nil)
	}
	return c.notReady(ctx, req, object, status, ReasonObserveFailed, err.Error(), err)
}

func (c *Controller) notReady(ctx context.Context, req ctlruntime.Request, object *v1alpha1.LocalRepo, status v1alpha1.AtprotoRepoBackendStatus,
	reason, message string, cause error) (ctlruntime.Result, error) {
	c.settle(req, false)
	status.Writable = false
	conds.Set(&status.Conditions, object.Generation, commonv1.ConditionReady, false, reason, message)
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, errors.Join(cause, err)
	}
	return ctlruntime.Result{}, cause
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.LocalRepo, status v1alpha1.AtprotoRepoBackendStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func validDID(raw string) bool {
	_, err := syntax.ParseDID(raw)
	return err == nil
}

type backend struct {
	Self commonv1.ObjectRef

	Labels map[string]string

	Pds commonv1.ObjectRef

	DID string
}

func (b backend) caller(ctx context.Context) context.Context {
	return bus.WithCallerLabels(ctx, b.Self, b.Labels)
}

func open(ctx context.Context, b *bus.Bus, repo backend, scope []string, network atprotorepo.RecordReader) (atprotorepo.Repo, error) {
	signer, err := bus.Resolve[atprotorepo.Repo](repo.caller(ctx), b, repo.Self, repo.Pds,
		bus.Capability{Name: pds.CapabilityAccount, Scope: []string{repo.DID}})
	if err != nil {
		return nil, err
	}
	return scoped.New(scoped.Route(signer, network), scope), nil
}

func accounts(ctx context.Context, b *bus.Bus, repo backend) (pds.Accounts, error) {
	return bus.Resolve[pds.Accounts](repo.caller(ctx), b, repo.Self, repo.Pds,
		bus.Capability{Name: pds.CapabilityAccount, Scope: []string{repo.DID}})
}

func syncer(ctx context.Context, b *bus.Bus, repo backend) (pds.Sync, error) {
	return bus.Resolve[pds.Sync](repo.caller(ctx), b, repo.Self, repo.Pds, bus.Capability{Name: pds.CapabilitySync})
}

func (c *Controller) release(target binding.Target) {
	c.backends.Set(target, nil)
	c.mu.Lock()
	delete(c.ready, target)
	c.mu.Unlock()
	c.prune()
}

func (c *Controller) follow(pdsKey string, pdsRef commonv1.ObjectRef) {
	if c.pds == nil {
		return
	}
	srv, ok := c.pds(pdsRef.Workspace, strings.TrimSpace(pdsRef.Name))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	current := c.feeds[pdsKey]
	if !ok || srv == nil {
		if current != nil {
			close(current.stop)
			delete(c.feeds, pdsKey)
		}
		return
	}
	if current != nil && current.srv == srv {
		return
	}
	if current != nil {
		close(current.stop)
	}
	feed := &commitFeed{srv: srv, stop: make(chan struct{})}
	c.feeds[pdsKey] = feed
	c.relays.Go(func() { c.relay(pdsKey, feed) })
}

func (c *Controller) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, feed := range c.feeds {
		if len(c.backends.Users(key)) == 0 {
			close(feed.stop)
			delete(c.feeds, key)
		}
	}
}

func (c *Controller) relay(pdsKey string, feed *commitFeed) {
	for {
		live, cancel := feed.srv.Sequencer().Subscribe()
		stopped := c.forward(pdsKey, feed, live)
		cancel()
		if stopped {
			return
		}
		c.log.Warn("the Pds commit feed dropped the LocalRepo controller; it resubscribes", "pds", pdsKey)
		if !c.enqueue(pdsKey, "", feed.stop) {
			return
		}
	}
}

func (c *Controller) forward(pdsKey string, feed *commitFeed, live <-chan *atrepo.CommitEvent) bool {
	for {
		select {
		case <-feed.stop:
			return true
		case ev, ok := <-live:
			if !ok {
				return false
			}
			if !c.enqueue(pdsKey, ev.Repo.String(), feed.stop) {
				return true
			}
		}
	}
}

func (c *Controller) enqueue(pdsKey, did string, stop <-chan struct{}) bool {
	for _, t := range c.backends.Users(pdsKey) {
		if did != "" {
			b, ok := c.backends.Get(t.Workspace, t.Name)
			if !ok || b.DID != did {
				continue
			}
		}
		select {
		case <-stop:
			return false
		case c.commits <- ctlruntime.Request{Cluster: t.Cluster, Name: t.Name}:
		}
	}
	return true
}

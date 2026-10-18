package providermain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/scaledriver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

type Deps struct {
	Registry clusterprovider.Clusters

	Decider *materialiser.Reconciler

	Host materialiser.Host

	NewInstance func() string

	Units *clusterprovider.UnitsHost

	Allocator *portallocator.Allocator

	Networks *subnetallocator.Allocator

	Tokens *tokenStore

	NodeScale *scaledriver.Driver

	NodeHosts *nodeHosts

	Tenant func(ctx context.Context, ref clusterprovider.Ref, cl *v1alpha1.Cluster) (*rest.Config, error)

	InstallTenant func(ctx context.Context, cluster string, cfg *rest.Config) error

	MountChild clusterprovider.MountChildWriter

	Homes *kcpclient.Client

	Links *linkRecorder

	Roller *clusterprovider.Roller
}

var ErrNoMountChild = errors.New(
	"no mount-child writer was supplied, so this pass would bring a cluster up and leave its " +
		"kubeconfig pointing at a workspace that proxies kcp itself. The child carries spec.URL, " +
		"which kcp accepts only from a system privileged identity, so it is written by a client " +
		"of its own and never by the identity that runs the tree")

var ErrNoTenant = errors.New(
	"no tenant credential seam was supplied, so the bootstrap cannot reach a cluster it " +
		"provisions. A nil here means the composition root skipped that construction, not " +
		"that something is missing from the repo")

var ErrNoHost = errors.New(
	"a join was asked for and no process lifecycle was supplied, so the pass that writes the " +
		"status the cluster earned would be observe-only: the cluster would be serving and the " +
		"CR would still read \"\"")

type Provider struct {
	cfg      Config
	provider *clusterprovider.Provider
	alloc    *portallocator.Allocator
	networks *subnetallocator.Allocator
	tokens   *tokenStore
	registry clusterprovider.Clusters

	units *clusterprovider.UnitsHost

	nodeScale *scaledriver.Driver

	nodes *nodeHosts

	host materialiser.Host

	links *linkRecorder

	homes   *kcpclient.Client
	sweptAt time.Time

	newInstance func() string
	log         *slog.Logger

	busyMu  sync.Mutex
	busy    map[string]bool
	deletes map[string]*deleteBackoff
	workers sync.WaitGroup
	bringUp chan struct{}
}

const MaxConcurrentBringUps = 4

const ShutdownTimeout = 60 * time.Second

func (p *Provider) Shutdown() error {
	if p.cfg.ObserveOnly() || p.host == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	p.log.Info("cluster provider: shutting down; every cluster's member units and Postgres object keep running and the next provider adopts them",
		"join", p.cfg.Join)
	return p.reapNodeHosts(ctx)
}

func (p *Provider) removeStore(ctx context.Context, key string) error {
	if p.units == nil {
		return nil
	}
	var errs []error
	if err := p.units.DeleteStore(ctx, key); err != nil {
		errs = append(errs, fmt.Errorf("deleting the Postgres object: %w", err))
	}
	if err := p.units.PurgeData(key); err != nil {
		errs = append(errs, fmt.Errorf("purging the data directory: %w", err))
	}
	if p.tokens != nil {
		p.tokens.forget(key)
	}
	return errors.Join(errs...)
}

func (p *Provider) finishDelete(ctx context.Context, ref clusterprovider.Ref, key string) error {
	if p.units == nil {
		return nil
	}
	gone, detail, err := p.units.StoreRemoval(ctx, key)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("waiting for the store of the cluster to go: %s", detail)
	}
	if err := p.alloc.Release(key); err != nil {
		return err
	}
	if p.networks != nil {
		if err := p.networks.Release(key); err != nil {
			return err
		}
	}
	if err := p.registry.RemoveFinalizer(ctx, ref); err != nil {
		return err
	}
	homeRemoved := false
	if p.homes != nil {
		ok, err := p.homes.RemoveHome(ctx, ref.Home)
		if err != nil {
			p.log.Warn("cluster provider: removing the home workspace of a deleted cluster did not complete; the sweep retries",
				"cluster", ref.Name, "key", key, "home", ref.Home, "error", err.Error())
		}
		homeRemoved = ok
	}
	p.log.Info("cluster provider: the deleted cluster is gone: ports and subnet released, finalizer removed",
		"cluster", ref.Name, "key", key, "home", ref.Home, "homeRemoved", homeRemoved)
	return nil
}

func (p *Provider) reapNodeHosts(ctx context.Context) error {
	if p.nodes == nil {
		p.log.Info("cluster provider: no node hosts to reap on shutdown",
			"cluster", p.cfg.Join,
			"reason", "this gateway built none: either no node-scale driver was attached or no "+
				"pass ever provisioned a node, so there is no agent of ours for a restart to find")
		return nil
	}
	clusters := p.nodes.clusters()
	p.log.Info("cluster provider: reaping the node hosts, after the group",
		"cluster", p.cfg.Join, "nodeClusters", clusters,
		"reason", "a node's guest is not a Step of the group, so nothing in the group's reap "+
			"reaches it: left running it keeps naming its node at the cluster's apiserver, and "+
			"the record it leaves makes the next gateway's Provision answer ErrAlreadyRunning, "+
			"so that cluster can never provision again")
	if err := p.nodes.reap(ctx); err != nil {
		return fmt.Errorf("clusterprovider: reaping the node hosts on shutdown: %w", err)
	}
	return nil
}

func Build(cfg Config, d Deps, log *slog.Logger) (*Provider, error) {
	if d.Registry == nil {
		return nil, errors.New("clusterprovider: Deps.Registry is required")
	}
	if d.Decider == nil {
		return nil, clusterprovider.ErrNoDecider
	}
	if d.Allocator == nil {
		return nil, errors.New("clusterprovider: Deps.Allocator is required; the lease file is the authority on which cluster holds which port")
	}
	if !cfg.ObserveOnly() {
		if d.Tenant == nil {
			return nil, ErrNoTenant
		}
		if d.Host == nil {
			return nil, ErrNoHost
		}
		if d.NewInstance == nil {
			return nil, errors.New("clusterprovider: Deps.NewInstance is required; status.instance is the only durable discriminator there is")
		}
		if d.MountChild == nil {
			return nil, ErrNoMountChild
		}
	}
	if log == nil {
		log = slog.Default()
	}

	opts := clusterprovider.Options{
		Registry: d.Registry,
		Decider:  d.Decider,

		Host: d.Host,

		WriteStatus: !cfg.ObserveOnly(),

		MountChild: d.MountChild,

		Ports: d.Allocator,

		NodeExpirySource: clusterprovider.NodeExpiryRollup,

		Interval: cfg.Interval,
		Log:      log,
	}
	if !cfg.ObserveOnly() {
		opts.Tenant, opts.GrantPath = d.Tenant, cfg.GrantPath
		opts.InstallTenant = d.InstallTenant
	}
	provider, err := clusterprovider.New(opts)
	if err != nil {
		return nil, err
	}

	p := &Provider{
		cfg:         cfg,
		provider:    provider,
		alloc:       d.Allocator,
		networks:    d.Networks,
		tokens:      d.Tokens,
		registry:    d.Registry,
		units:       d.Units,
		nodeScale:   d.NodeScale,
		nodes:       d.NodeHosts,
		host:        d.Host,
		links:       d.Links,
		homes:       d.Homes,
		newInstance: d.NewInstance,
		log:         log,
	}
	p.report()
	return p, nil
}

func (p *Provider) Run(ctx context.Context) error {
	if p.cfg.ObserveOnly() {
		p.log.Info("cluster provider: observing only", "join", "unset: no process lifecycle is attached, so no pass may act or write")
		return p.provider.Run(ctx)
	}
	if p.cfg.AllClusters() {
		return p.joinAll(ctx)
	}
	return p.join(ctx)
}

func (p *Provider) join(ctx context.Context) error {
	ref, _, err := p.lookup(ctx, p.cfg.Join)
	if err != nil {
		return err
	}
	key, err := ref.Key()
	if err != nil {
		return err
	}
	dsn, err := p.store(ctx, key)
	if err != nil {
		return err
	}
	p.log.Info("cluster provider: joining", "cluster", ref.Name, "key", key, "home", ref.Home,
		"datastore", dsn)

	if err := p.resetSnapshots([]string{key}); err != nil {
		return err
	}
	p.adoptNodes(ctx, []string{key})

	if p.nodeScale == nil {
		p.log.Warn("cluster provider: no node-scale driver; the cluster will not scale its nodes")
	}
	tick := time.NewTicker(p.cfg.Interval)
	defer tick.Stop()
	for {
		p.pass(ctx, ref, key)
		p.scale(ctx, key, ref.Name, true)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (p *Provider) resetSnapshots(keys []string) error {
	if p.nodes == nil {
		return nil
	}
	for _, key := range keys {
		if err := p.nodes.resetSnapshot(key); err != nil {
			return fmt.Errorf("clusterprovider: writing the empty instance snapshot for %s before "+
				"any node exists: %w. The cloud controller manager reads that file to decide which "+
				"Nodes still have a guest; the provider always writes the file rather than leaving "+
				"its absence to mean something", key, err)
		}
	}
	return nil
}

func (p *Provider) adoptNodes(ctx context.Context, keys []string) {
	if p.nodes == nil {
		return
	}
	for _, key := range keys {
		if err := p.nodes.adopt(ctx, key); err != nil {
			p.log.Warn("cluster provider: adopting the live market nodes of a cluster at startup did "+
				"not complete; its next pass tries again", "key", key, "error", err.Error())
		}
	}
}

func (p *Provider) joinAll(ctx context.Context) error {
	p.log.Info("cluster provider: joining every Cluster in the tree", "join", p.cfg.Join,
		"passes", "serial, one cluster at a time",
		"reason", "a failure in one cluster is logged and the pass moves on, so one cluster that "+
			"cannot come up never holds back the others")
	refs, err := p.registry.Refs(ctx)
	if err != nil {
		return fmt.Errorf("clusterprovider: listing the Cluster CRs before the first all-clusters "+
			"pass: %w", err)
	}
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		key, err := ref.Key()
		if err != nil {
			return err
		}
		keys = append(keys, key)
	}
	if err := p.resetSnapshots(keys); err != nil {
		return err
	}
	p.adoptNodes(ctx, keys)
	if p.nodeScale == nil {
		p.log.Warn("cluster provider: no node-scale driver; no cluster will scale its nodes")
	}
	tick := time.NewTicker(p.cfg.Interval)
	defer tick.Stop()
	for {
		p.passAll(ctx)
		select {
		case <-ctx.Done():
			p.workers.Wait()
			return nil
		case <-tick.C:
		}
	}
}

func (p *Provider) passAll(ctx context.Context) {
	refs, err := p.registry.Refs(ctx)
	if err != nil {
		p.log.Error("cluster provider: listing the Cluster CRs in the tree failed; no cluster was "+
			"touched this pass", "error", err.Error())
		return
	}
	p.provider.Reclaim(refs)
	p.sweepHomes(ctx, refs)
	for _, ref := range refs {
		key, err := ref.Key()
		if err != nil {
			p.log.Error("cluster provider: this cluster has no derivable key; its pass is skipped",
				"cluster", ref.Name, "home", ref.Home, "error", err.Error())
			continue
		}
		if !p.claim(key) {
			continue
		}
		p.workers.Add(1)
		go func(ref clusterprovider.Ref, key string) {
			defer p.workers.Done()
			defer p.release(key)
			p.runOne(ctx, ref)
		}(ref, key)
	}
}

const (
	HomeSweepEvery = time.Minute

	EmptyHomeGrace = 10 * time.Minute
)

func (p *Provider) sweepHomes(ctx context.Context, refs []clusterprovider.Ref) {
	if p.homes == nil || time.Since(p.sweptAt) < HomeSweepEvery {
		return
	}
	p.sweptAt = time.Now()
	live := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if key, err := ref.Key(); err == nil {
			live[key] = true
		}
	}
	dropped, err := p.homes.SweepAgentHomes(ctx, live)
	if len(dropped) > 0 {
		p.log.Info("cluster provider: removed node claim index entries whose cluster is gone",
			"removed", len(dropped), "live", len(live), "keys", dropped)
	}
	if err != nil {
		p.log.Warn("cluster provider: the node claim index sweep did not complete", "error", err.Error())
	}
	removed, err := p.homes.SweepHomes(ctx, live, EmptyHomeGrace)
	if len(removed) > 0 {
		p.log.Info("cluster provider: removed home workspaces that hold no cluster and no node claim",
			"removed", len(removed), "live", len(live), "homes", removed, "grace", EmptyHomeGrace.String())
	}
	if err != nil {
		p.log.Warn("cluster provider: the empty home workspace sweep did not complete", "error", err.Error())
	}
}

func (p *Provider) claim(key string) bool {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	if p.busy == nil {
		p.busy = map[string]bool{}
	}
	if p.busy[key] {
		return false
	}
	p.busy[key] = true
	return true
}

func (p *Provider) release(key string) {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	delete(p.busy, key)
}

func (p *Provider) runOne(ctx context.Context, ref clusterprovider.Ref) {
	if err := p.one(ctx, ref); err != nil {
		p.log.Error("cluster provider: this cluster's pass failed and the others continue",
			"cluster", ref.Name, "home", ref.Home, "error", err.Error())
	}
}

func (p *Provider) bringUpSlot(ctx context.Context) (func(), bool) {
	p.busyMu.Lock()
	if p.bringUp == nil {
		p.bringUp = make(chan struct{}, MaxConcurrentBringUps)
	}
	slots := p.bringUp
	p.busyMu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

func (p *Provider) one(ctx context.Context, ref clusterprovider.Ref) error {
	key, err := ref.Key()
	if err != nil {
		return err
	}
	cl, err := p.registry.Read(ctx, ref)
	if err != nil {
		return fmt.Errorf("clusterprovider: reading the Cluster CR of %s out of %s: %w",
			ref.Name, ref.Home, err)
	}
	if cl.DeletionTimestamp != nil {
		return p.teardown(ctx, ref, key, cl)
	}
	if !cl.Spec.Running {
		p.log.Info("cluster provider: this cluster is not running, so its group is reaped and "+
			"nothing is brought up for it", "cluster", ref.Name, "key", key, "home", ref.Home)
		return p.reapOne(ctx, key)
	}
	if p.units != nil {
		if err := p.units.StartStore(ctx, key); err != nil {
			return err
		}
	}
	done, ok := p.bringUpSlot(ctx)
	if !ok {
		return ctx.Err()
	}
	if p.pass(ctx, ref, key) {
		p.pass(ctx, ref, key)
	}
	done()
	p.scale(ctx, key, ref.Name, true)
	return nil
}

func (p *Provider) reapOne(ctx context.Context, key string) error {
	if p.nodes != nil {
		if err := p.nodes.adoptOnce(ctx, key); err != nil {
			return fmt.Errorf("clusterprovider: adopting the live market nodes of %s before reaping it: "+
				"%w -- a claim nothing holds is a machine nothing releases", key, err)
		}
		if err := p.nodes.reapCluster(ctx, key); err != nil {
			return err
		}
	}
	return p.host.Reap(ctx, key)
}

func (p *Provider) scale(ctx context.Context, key, name string, running bool) {
	if p.nodeScale == nil || !running {
		return
	}
	if p.nodes != nil {
		if err := p.nodes.reconcile(ctx, key); err != nil {
			p.log.Warn("cluster provider: reconciling the Nodes whose machines the door has taken "+
				"away did not complete", "cluster", name, "key", key, "error", err.Error())
		}
		outcome, err := p.nodes.confirm(ctx, key)
		if err != nil {
			p.log.Warn("cluster provider: confirming the nodes this cluster already holds did not "+
				"complete", "cluster", name, "key", key, "error", err.Error())
		}
		if len(outcome.joined) > 0 {
			p.nodeScale.Joined(key)
		}
		if reason := outcome.declinedReason(); reason != "" {
			p.nodeScale.Declined(key, reason)
		}
	}
	dec, err := p.nodeScale.RunPass(ctx, key)
	b := p.nodeScale.State(key).Backoff
	p.provider.SetNodeBackoff(key, clusterprovider.NodeBackoff{Ends: b.Ends, Until: b.Until, Reason: b.Reason})
	if err != nil {
		p.log.Warn("cluster provider: the node-scale pass did not complete",
			"cluster", name, "key", key, "error", err.Error())
		return
	}
	args := []any{"cluster", name, "key", key,
		"action", dec.Action.String(), "nodes", dec.Nodes, "reason", dec.Reason}
	if !dec.BackoffUntil.IsZero() {
		args = append(args, "backoffUntil", dec.BackoffUntil.UTC().Format(time.RFC3339))
	}
	p.log.Info("cluster provider: node scale", args...)
}

func (p *Provider) pass(ctx context.Context, ref clusterprovider.Ref, key string) bool {
	pass, err := p.provider.Reconcile(ctx, ref)
	if pass.Result.Phase != "" {
		args := []any{
			"cluster", ref.Name, "key", key, "home", ref.Home,
			"phase", pass.Result.Phase, "url", pass.Result.URL, "instance", pass.Result.Instance,
			"wrote", pass.Wrote, "alert", pass.Alert(),
		}
		p.log.Info("cluster provider: status", args...)
		if pass.Result.Alert != "" {
			p.log.Warn("cluster provider: the materialiser reported an alert",
				"cluster", ref.Name, "key", key, "alert", pass.Result.Alert)
		}
	}
	if pass.Unfinalized {
		p.removeLinks(ctx, ref, key)
		if err := p.removeStore(ctx, key); err != nil {
			p.log.Warn("cluster provider: removing the store of an unfinalized cluster did not complete",
				"cluster", ref.Name, "key", key, "error", err.Error())
		}
	}
	if err != nil {
		p.log.Error("cluster provider: the pass failed", "cluster", ref.Name, "key", key,
			"error", err.Error())
	}
	return slices.Contains(pass.Result.Ops, materialiser.OpSpawn)
}

func (p *Provider) store(ctx context.Context, cluster string) (string, error) {
	if p.units == nil {
		return p.cfg.StoreEndpoint, nil
	}
	s, err := p.units.EnsureStore(ctx, cluster)
	if err != nil {
		return "", err
	}
	if p.cfg.StoreEndpoint != "" {
		return p.cfg.StoreEndpoint, nil
	}
	return s.DSN(), nil
}

func (p *Provider) lookup(ctx context.Context, name string) (clusterprovider.Ref, *v1alpha1.Cluster, error) {
	refs, err := p.registry.Refs(ctx)
	if err != nil {
		return clusterprovider.Ref{}, nil, err
	}
	var named []clusterprovider.Ref
	for _, ref := range refs {
		if ref.Name == name {
			named = append(named, ref)
		}
	}
	switch len(named) {
	case 1:
	case 0:
		found := make([]string, 0, len(refs))
		for _, ref := range refs {
			found = append(found, ref.Home+"/"+ref.Name)
		}
		return clusterprovider.Ref{}, nil, fmt.Errorf(
			"clusterprovider: no Cluster %q in the tree this credential can see; it holds %d: %v",
			name, len(found), found)
	default:
		homes := make([]string, 0, len(named))
		for _, ref := range named {
			homes = append(homes, ref.Home)
		}
		return clusterprovider.Ref{}, nil, fmt.Errorf(
			"clusterprovider: %d workspaces in this tree hold a Cluster named %q (%v), and %s names "+
				"one cluster. A name is unique only inside its home workspace, so this join is "+
				"refused rather than resolved to whichever home was listed first: the two clusters "+
				"have different state dirs, different ports and different node names, and picking "+
				"one silently would provision the wrong cluster. Set %s to %s to reconcile every "+
				"cluster in the tree instead",
			len(named), name, homes, envJoin, envJoin, JoinAllClusters)
	}
	ref := named[0]
	cl, err := p.registry.Read(ctx, ref)
	if err != nil {
		return clusterprovider.Ref{}, nil, err
	}
	key, err := ref.Key()
	if err != nil {
		return clusterprovider.Ref{}, nil, err
	}
	p.log.Info("cluster provider: joining", "cluster", ref.Name, "key", key, "home", ref.Home,
		"did", ref.DID, "account", ref.Account,
		"running", cl.Spec.Running, "phase", cl.Status.Phase, "url", cl.Status.URL)
	return ref, cl, nil
}

func (p *Provider) report() {
	if p.cfg.ObserveOnly() {
		p.log.Info("cluster provider: report",
			"mode", "observe",
			"decides", "yes -- the phase machine is materialiser's",
			"acts", "no -- Options.Host is nil, so no pass may spawn, reap or write",
			"bootstraps", "not in this mode: the bootstrap is reached through a WRITE pass, and observe-only returns before it",
			"consequence", "the tenant grant is not applied while this mode is set")
		return
	}
	mode := "join"
	scope := p.cfg.Join
	status := "on, for THIS cluster only: the join holds its VM, so the pass may write"
	if p.cfg.AllClusters() {
		mode = "join-all"
		scope = "every Cluster in the tree"
		status = "on, for EVERY cluster this pass brings up: each one holds its own VM"
	}
	p.log.Info("cluster provider: report",
		"mode", mode,
		"join", scope,
		"key", "(derived: <accountHash>-<name>, from the cluster's home workspace)",
		"instance", "(derived: pdr-<key>)",
		"datastore", p.storePlan(),
		"datastoreHasRow", p.cfg.DatastoreHasRow,
		"mountContext", p.cfg.MountContext,
		"mountChild", "written on every pass by a client built from that kcp context, because "+
			"kcp accepts spec.URL only from a system privileged user and the identity that runs "+
			"the tree is not one",
		"statusWrite", status,
		"bootstrap", "runs on each cluster's own transition into Ready, once")
}

func (p *Provider) storePlan() string {
	if p.cfg.Datastore != "" {
		return p.cfg.Datastore + " (supplied by the operator)"
	}
	return "one Postgres object per cluster, placed by the HostProcess supervisor as " + p.cfg.UnitPrefix + "pg-<key>.service"
}

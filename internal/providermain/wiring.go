package providermain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterkcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	clustercontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/cluster"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/scaledriver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

type Deps struct {
	Rest *rest.Config

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

	Tenant func(ctx context.Context, ref clusterprovider.Ref, cl *v1alpha1.Cluster) (*clusterprovider.Credential, error)

	Tenants func(ctx context.Context, cluster string) (kubernetes.Interface, error)

	InstallTenant func(ctx context.Context, cluster string, cred *clusterprovider.Credential) error

	MountChild clusterprovider.MountChildWriter

	Homes *kcpclient.Client

	Links *linkRecorder

	ClusterLinks *clusterprovider.Links

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
	rest     *rest.Config
	provider *clusterprovider.Provider
	alloc    *portallocator.Allocator
	networks *subnetallocator.Allocator
	tokens   *tokenStore
	registry clusterprovider.Clusters

	units *clusterprovider.UnitsHost

	nodeScale *scaledriver.Driver

	nodes *nodeHosts

	host materialiser.Host

	links      *linkRecorder
	linkWrites *clusterprovider.Links

	roller *clusterprovider.Roller

	homes *kcpclient.Client

	newInstance func() string
	log         *slog.Logger

	changes chan string

	tenants *tenantWatch

	sweeps chan struct{}

	busyMu  sync.Mutex
	deletes map[string]*deleteBackoff
	bringUp chan struct{}
}

const MaxConcurrentBringUps = 4

const ShutdownTimeout = 60 * time.Second

func (p *Provider) Shutdown(ctx context.Context) error {
	if p.cfg.ObserveOnly() || p.host == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	p.log.Info("cluster provider: shutting down; every cluster's member units and Postgres object keep running and the next provider adopts them",
		"join", p.cfg.Join)
	err := p.reapNodeHosts(ctx)
	if p.linkWrites != nil {
		err = errors.Join(err, p.linkWrites.Wait(ctx))
	}
	return err
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

		Log: log,
	}
	if !cfg.ObserveOnly() {
		opts.Tenant, opts.GrantPath = d.Tenant, cfg.GrantPath
		opts.InstallTenant = d.InstallTenant
		opts.Grant = clusterkcp.Grant
	}
	provider, err := clusterprovider.New(opts)
	if err != nil {
		return nil, err
	}

	p := &Provider{
		cfg:         cfg,
		rest:        d.Rest,
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
		linkWrites:  d.ClusterLinks,
		homes:       d.Homes,
		roller:      d.Roller,
		newInstance: d.NewInstance,
		log:         log,
		changes:     make(chan string, ChangeBuffer),
		sweeps:      make(chan struct{}, 1),
	}
	if d.Tenants != nil && d.NodeScale != nil {
		p.tenants = newTenantWatch(d.Tenants, p.changed, log)
	}
	if d.Rest == nil {
		return nil, errors.New("clusterprovider: Deps.Rest is required; the provider is a controller on the clusters APIExport")
	}
	p.report()
	return p, nil
}

const ChangeBuffer = 256

func (p *Provider) Run(ctx context.Context) error {
	mgr, err := ctlruntime.NewManager(ctlruntime.Config{Rest: p.rest, AddToScheme: []func(*kruntime.Scheme) error{v1alpha1.AddToScheme}, Log: p.log})
	if err != nil {
		return fmt.Errorf("clusterprovider: the controller manager: %w", err)
	}
	opts := clustercontroller.ProviderOptions{Pass: p, Changes: p.changes}
	switch {
	case p.cfg.ObserveOnly():
		p.log.Info("cluster provider: observing only", "join", "unset: no process lifecycle is attached, so no pass may act or write")
		p.reclaimAll(ctx)
	case p.cfg.AllClusters():
		if err := p.startAll(ctx); err != nil {
			return err
		}
	default:
		joined, err := p.startJoin(ctx)
		if err != nil {
			return err
		}
		opts.Keep = func(ref clusterprovider.Ref) bool { return ref.Home == joined.Home && ref.Name == joined.Name }
	}
	if _, err := clustercontroller.RegisterProvider(mgr, opts); err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	if p.homes != nil && !p.cfg.ObserveOnly() {
		wg.Go(func() { p.sweeper(sctx) })
	}
	err = mgr.Start(ctx)
	stop()
	if p.tenants != nil {
		p.tenants.stopAll()
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (p *Provider) changed(key string) {
	select {
	case p.changes <- key:
	default:
	}
}

func (p *Provider) Pass(ctx context.Context, ref clusterprovider.Ref, _ *v1alpha1.Cluster) (time.Duration, error) {
	key, err := ref.Key()
	if err != nil {
		return 0, err
	}
	if p.roller != nil {
		defer p.roller.Note(key)
	}
	switch {
	case p.cfg.ObserveOnly():
		return p.observe(ctx, ref)
	case p.cfg.AllClusters():
		return p.one(ctx, ref, key)
	}
	res, err := p.passes(ctx, ref, key)
	return soonest(res.wake, p.scaled(ctx, key, ref.Name, res.phase)), err
}

func (p *Provider) scaled(ctx context.Context, key, name string, phase v1alpha1.ClusterPhase) time.Duration {
	wake, failed := p.scale(ctx, key, name, true)
	if failed && phase == v1alpha1.ClusterReady {
		return soonest(wake, ScaleRetry)
	}
	return wake
}

func (p *Provider) Gone(ctx context.Context, ref clusterprovider.Ref) {
	key, err := ref.Key()
	if err == nil {
		if p.tenants != nil {
			p.tenants.stop(key)
		}
		p.provider.ForgetAddOns(key)
	}
	p.log.Info("cluster provider: the Cluster CR is gone", "cluster", ref.Name, "home", ref.Home, "key", key)
	p.reclaimAll(ctx)
	p.ClaimGone(ctx)
}

func (p *Provider) ClaimGone(context.Context) {
	select {
	case p.sweeps <- struct{}{}:
	default:
	}
}

func (p *Provider) observe(ctx context.Context, ref clusterprovider.Ref) (time.Duration, error) {
	pass, err := p.provider.Reconcile(ctx, ref)
	if err != nil {
		p.log.Error("reconcile failed", "cluster", ref.Name, "home", ref.Home, "err", err)
		return 0, err
	}
	p.log.Info("reconciled",
		"cluster", ref.Name,
		"home", ref.Home,
		"did", ref.DID,
		"running", pass.Cluster.Spec.Running,
		"phase", pass.Result.Phase,
		"url", pass.Result.URL,
		"foreign", pass.Foreign,
		"observeOnly", pass.ObserveOnly,
		"wrote", pass.Wrote,
		"alert", pass.Alert(),
	)
	return 0, nil
}

func (p *Provider) startJoin(ctx context.Context) (clusterprovider.Ref, error) {
	ref, _, err := p.lookup(ctx, p.cfg.Join)
	if err != nil {
		return ref, err
	}
	key, err := ref.Key()
	if err != nil {
		return ref, err
	}
	dsn, err := p.store(ctx, key)
	if err != nil {
		return ref, err
	}
	p.log.Info("cluster provider: joining", "cluster", ref.Name, "key", key, "home", ref.Home,
		"datastore", dsn)
	if err := p.resetSnapshots([]string{key}); err != nil {
		return ref, err
	}
	p.adoptNodes(ctx, []string{key})
	if p.nodeScale == nil {
		p.log.Warn("cluster provider: no node-scale driver; the cluster will not scale its nodes")
	}
	return ref, nil
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

func (p *Provider) startAll(ctx context.Context) error {
	p.log.Info("cluster provider: joining every Cluster in the tree", "join", p.cfg.Join,
		"passes", "one reconcile per Cluster on the clusters APIExport, driven by watches",
		"reason", "a failure in one cluster backs off that cluster alone, so one cluster that "+
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
	p.provider.Reclaim(refs)
	if p.nodeScale == nil {
		p.log.Warn("cluster provider: no node-scale driver; no cluster will scale its nodes")
	}
	return nil
}

func (p *Provider) reclaimAll(ctx context.Context) {
	refs, err := p.registry.Refs(ctx)
	if err != nil {
		p.log.Error("cluster provider: listing the Cluster CRs in the tree failed; no lease was "+
			"reclaimed", "error", err.Error())
		return
	}
	p.provider.Reclaim(refs)
}

const (
	EmptyHomeGrace = 10 * time.Minute
)

func (p *Provider) sweeper(ctx context.Context) {
	var grace *time.Timer
	var graceC <-chan time.Time
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	p.sweepHomes(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.sweeps:
			if grace == nil {
				grace = time.NewTimer(EmptyHomeGrace + time.Second)
			} else {
				grace.Reset(EmptyHomeGrace + time.Second)
			}
			graceC = grace.C
		case <-graceC:
			graceC = nil
		}
		p.sweepHomes(ctx)
	}
}

func (p *Provider) sweepHomes(ctx context.Context) {
	refs, err := p.registry.Refs(ctx)
	if err != nil {
		p.log.Warn("cluster provider: listing the Cluster CRs before the home sweep failed; nothing was swept", "error", err.Error())
		return
	}
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

func (p *Provider) one(ctx context.Context, ref clusterprovider.Ref, key string) (time.Duration, error) {
	cl, err := p.registry.Read(ctx, ref)
	if err != nil {
		return 0, fmt.Errorf("clusterprovider: reading the Cluster CR of %s out of %s: %w",
			ref.Name, ref.Home, err)
	}
	if cl.DeletionTimestamp != nil {
		if p.tenants != nil {
			p.tenants.stop(key)
		}
		return p.teardown(ctx, ref, key, cl)
	}
	if !cl.Spec.Running {
		if p.tenants != nil {
			p.tenants.stop(key)
		}
		p.log.Info("cluster provider: this cluster is not running, so its group is reaped and "+
			"nothing is brought up for it", "cluster", ref.Name, "key", key, "home", ref.Home)
		return 0, p.reapOne(ctx, key)
	}
	if p.units != nil {
		if err := p.units.StartStore(ctx, key); err != nil {
			return 0, err
		}
	}
	done, ok := p.bringUpSlot(ctx)
	if !ok {
		return 0, ctx.Err()
	}
	res, err := p.passes(ctx, ref, key)
	done()
	return soonest(res.wake, p.scaled(ctx, key, ref.Name, res.phase)), err
}

type passOutcome struct {
	wake time.Duration

	phase v1alpha1.ClusterPhase
}

func (p *Provider) passes(ctx context.Context, ref clusterprovider.Ref, key string) (passOutcome, error) {
	out, again, err := p.pass(ctx, ref, key)
	if again && err == nil {
		out, _, err = p.pass(ctx, ref, key)
	}
	if p.tenants != nil && out.phase == v1alpha1.ClusterReady {
		p.tenants.ensure(key)
	}
	return out, err
}

func soonest(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0 || a < b:
		return a
	}
	return b
}

func until(at time.Time) time.Duration {
	if at.IsZero() {
		return 0
	}
	return max(time.Until(at), time.Second)
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

func (p *Provider) scale(ctx context.Context, key, name string, running bool) (time.Duration, bool) {
	if p.nodeScale == nil || !running {
		return 0, false
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
	wake := until(p.nodeScale.Wake(key))
	if p.nodes != nil {
		wake = soonest(wake, until(p.nodes.wake(key)))
	}
	if err != nil {
		p.log.Warn("cluster provider: the node-scale pass did not complete",
			"cluster", name, "key", key, "error", err.Error())
		return wake, true
	}
	args := []any{"cluster", name, "key", key,
		"action", dec.Action.String(), "nodes", dec.Nodes, "reason", dec.Reason}
	if !dec.BackoffUntil.IsZero() {
		args = append(args, "backoffUntil", dec.BackoffUntil.UTC().Format(time.RFC3339))
	}
	p.log.Info("cluster provider: node scale", args...)
	return wake, false
}

const ScaleRetry = 10 * time.Second

func (p *Provider) pass(ctx context.Context, ref clusterprovider.Ref, key string) (passOutcome, bool, error) {
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
	out := passOutcome{wake: pass.Result.RequeueAfter, phase: pass.Result.Phase}
	if due, ok := p.provider.AddOnsDueIn(ref); ok && pass.Result.Phase == v1alpha1.ClusterReady {
		out.wake = soonest(out.wake, due)
	}
	if err != nil {
		p.log.Error("cluster provider: the pass failed", "cluster", ref.Name, "key", key,
			"error", err.Error())
		return out, false, err
	}
	if slices.Contains(pass.Result.Ops, materialiser.OpSpawnFailed) {
		return out, false, fmt.Errorf("clusterprovider: the process group of %s did not come up: %s", ref.Name, pass.Result.Alert)
	}
	return out, pass.Result.Again || slices.Contains(pass.Result.Ops, materialiser.OpSpawn), nil
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

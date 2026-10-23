package clusterprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

var readyPhase = func() v1alpha1.ClusterPhase {
	settled, err := revocation.WakeSettled(revocation.Effect{State: v1alpha1.IdentityRevoked})
	if err != nil {
		panic("clusterprovider: revocation.WakeSettled refused a terminal effect: " + err.Error())
	}
	phase, err := revocation.Ready(settled)
	if err != nil {
		panic("clusterprovider: revocation.Ready refused a Settled: " + err.Error())
	}
	return phase
}()

const DefaultInterval = 2 * time.Second

const DefaultAddOnInterval = 30 * time.Minute

var ErrNoDecider = errors.New("clusterprovider: Decider is required: the phase machine is internal/materialiser's, not this package's")

type Clusters interface {
	Refs(ctx context.Context) ([]Ref, error)

	Read(ctx context.Context, ref Ref) (*v1alpha1.Cluster, error)

	WriteStatus(ctx context.Context, ref Ref, st v1alpha1.ClusterStatus) error

	TenantPath(ctx context.Context, ref Ref) materialiser.PathProbe

	IdentityState(ctx context.Context, ref Ref) (v1alpha1.IdentityState, error)

	Nodes(ctx context.Context, ref Ref) ([]v1alpha1.Node, error)

	WriteNode(ctx context.Context, ref Ref, nodeName string, status v1alpha1.NodeStatus) error

	RemoveFinalizer(ctx context.Context, ref Ref) error
}

type MountChildWriter interface {
	EnsureMountChild(ctx context.Context, home, name, url string) error
}

var _ Clusters = (*Registry)(nil)

type NodeExpirySource func(cl *v1alpha1.Cluster, nodes []v1alpha1.Node) (expiry int64, reported bool)

const NodeExpiryUnlimited int64 = 0

func NodeExpiryRollup(_ *v1alpha1.Cluster, nodes []v1alpha1.Node) (int64, bool) {
	live := 0
	rollup := NodeExpiryUnlimited
	for i := range nodes {
		if nodes[i].Status.Phase == v1alpha1.NodeDraining {
			continue
		}
		live++
		expiry := nodes[i].Status.NodeExpiry
		if expiry == nil {
			return 0, false
		}
		if *expiry == NodeExpiryUnlimited {
			continue
		}
		if rollup == NodeExpiryUnlimited || *expiry < rollup {
			rollup = *expiry
		}
	}
	if live == 0 {
		return NodeExpiryUnlimited, true
	}
	return rollup, true
}

type Options struct {
	Registry Clusters

	Decider *materialiser.Reconciler

	Ports *portallocator.Allocator

	Host materialiser.Host

	NodeExpirySource NodeExpirySource

	WriteStatus bool

	MountChild MountChildWriter

	OnReady func(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) error

	InstallTenant func(ctx context.Context, cluster string, cfg *rest.Config) error

	Tenant func(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) (*rest.Config, error)

	GrantPath string

	Probe func(ctx context.Context, url string) (int, bool)

	Interval time.Duration

	AddOnInterval time.Duration

	Now func() time.Time

	Log *slog.Logger
}

type Provider struct {
	opts Options

	mu sync.Mutex

	nodeBackoff map[string]NodeBackoff

	addOns map[string]time.Time
}

type NodeBackoff struct {
	Ends int

	Until time.Time

	Reason string
}

const ReasonMarketProvisionsFailing = "MarketProvisionsFailing"

const (
	conditionTenantBootstrapped = "TenantBootstrapped"

	reasonTenantBootstrapped = "TenantBootstrapped"
)

func (p *Provider) SetNodeBackoff(cluster string, b NodeBackoff) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.Ends <= 0 {
		delete(p.nodeBackoff, cluster)
		return
	}
	if p.nodeBackoff == nil {
		p.nodeBackoff = map[string]NodeBackoff{}
	}
	p.nodeBackoff[cluster] = b
}

func (p *Provider) withNodeBackoff(cluster string, conds []metav1.Condition, generation int64) []metav1.Condition {
	p.mu.Lock()
	b, held := p.nodeBackoff[cluster]
	p.mu.Unlock()
	out := make([]metav1.Condition, len(conds))
	copy(out, conds)
	if !held {
		meta.RemoveStatusCondition(&out, v1alpha1.ConditionNodeProvisionBackoff)
		return out
	}
	meta.SetStatusCondition(&out, metav1.Condition{
		Type:   v1alpha1.ConditionNodeProvisionBackoff,
		Status: metav1.ConditionTrue,
		Reason: ReasonMarketProvisionsFailing,
		Message: fmt.Sprintf("%d market provision(s) of this cluster in a row ended without a node; the next attempt "+
			"is not made before %s; the last end: %s", b.Ends, b.Until.UTC().Format(time.RFC3339), b.Reason),
		ObservedGeneration: generation,
	})
	return out
}

type Pass struct {
	Ref      Ref
	Cluster  *v1alpha1.Cluster
	Observed materialiser.Observed
	Result   materialiser.Result

	ObserveOnly bool

	Foreign bool

	Ratchet bool

	Wrote bool

	Unfinalized bool

	Err error
}

func (p Pass) Alert() string {
	switch {
	case p.Ratchet:
		return fmt.Sprintf(
			"cluster %s: phase is %s and this CR carries no deletionTimestamp; HELD -- Draining is a "+
				"one-way ratchet and nothing returns it to service",
			p.Ref.Name, p.Result.Phase)
	case p.Foreign:
		return fmt.Sprintf(
			"cluster %s: an apiserver answers at %s and this process holds no group for it; "+
				"left alone (spawning over it would be two servers on one advertise port)",
			p.Ref.Name, p.Cluster.Status.URL)
	case p.ObserveOnly:
		return fmt.Sprintf("cluster %s: observed only, no process lifecycle attached", p.Ref.Name)
	default:
		return fmt.Sprintf("cluster %s: phase %s", p.Ref.Name, p.Result.Phase)
	}
}

func New(opts Options) (*Provider, error) {
	if opts.Registry == nil {
		return nil, errors.New("clusterprovider: Registry is required")
	}
	if opts.Decider == nil {
		return nil, ErrNoDecider
	}
	if opts.Probe == nil {
		opts.Probe = Probe
	}
	if opts.Interval == 0 {
		opts.Interval = DefaultInterval
	}
	if opts.AddOnInterval <= 0 {
		opts.AddOnInterval = DefaultAddOnInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if (opts.Tenant != nil) != (opts.GrantPath != "") {
		return nil, ErrNoBootstrap
	}
	p := &Provider{opts: opts}
	if opts.Tenant != nil && opts.OnReady == nil {
		p.opts.OnReady = p.Bootstrap
	}
	return p, nil
}

type Established struct {
	URL string

	Instance string
}

func APIServerURL(port int) string {
	return fmt.Sprintf("https://127.0.0.1:%d", port)
}

func (p *Provider) Reconcile(ctx context.Context, ref Ref) (Pass, error) {
	return p.reconcile(ctx, ref, nil)
}

func (p *Provider) Joined(ctx context.Context, ref Ref, at Established) (Pass, error) {
	if at.URL == "" {
		return Pass{Ref: ref}, errors.New(
			"clusterprovider: Joined needs the address the apiserver answered at; a Ready with " +
				"an empty status.URL is the hard stop that loops the reconcile forever (K2)")
	}
	return p.reconcile(ctx, ref, func(o *materialiser.Observed) {
		o.Cluster.Status.URL = at.URL
		o.Cluster.Status.Instance = at.Instance
		_, o.Reachable = p.opts.Probe(ctx, at.URL)
	})
}

func (p *Provider) reconcile(ctx context.Context, ref Ref, with func(*materialiser.Observed)) (Pass, error) {
	cl, err := p.opts.Registry.Read(ctx, ref)
	if err != nil {
		return Pass{Ref: ref}, err
	}
	if err := p.reportNodeExpiry(ctx, ref, cl); err != nil {
		return Pass{Ref: ref}, err
	}
	o, err := p.observe(ctx, ref, cl)
	if err != nil {
		return Pass{Ref: ref, Cluster: cl}, err
	}
	if with != nil {
		with(&o)
	}
	pass := Pass{Ref: ref, Cluster: cl, Observed: o}

	if cl.Status.Phase == v1alpha1.ClusterDraining && cl.DeletionTimestamp == nil {
		pass.Ratchet = true
		pass.Result = carryStatus(cl)
		return pass, nil
	}

	if p.opts.Host == nil {
		pass.ObserveOnly = true
		pass.Result = carryStatus(cl)
		return pass, nil
	}
	if !p.opts.Host.Live(o.Cluster.Name) && o.Reachable && cl.Status.URL != "" {
		pass.Foreign = true
		pass.Result = carryStatus(cl)
		return pass, nil
	}

	if p.opts.WriteStatus && cl.Spec.Running && cl.DeletionTimestamp == nil &&
		(cl.Status.Phase == "" || cl.Status.Phase == v1alpha1.ClusterUnavailable) && !p.opts.Host.Live(o.Cluster.Name) {
		early := cl.Status
		early.Phase = v1alpha1.ClusterInitializing
		if err := p.opts.Registry.WriteStatus(ctx, ref, early); err != nil {
			p.opts.Log.Warn("cluster provider: writing Initializing before the members start failed; the pass goes on",
				"cluster", ref.Name, "home", ref.Home, "error", err.Error())
		} else {
			cl.Status = early
		}
	}

	res, err := p.opts.Decider.Reconcile(ctx, o)
	if err != nil {
		return pass, err
	}
	res.Conditions = p.withNodeBackoff(o.Cluster.Name, res.Conditions, cl.Generation)
	pass.Result = res
	if !p.opts.WriteStatus {
		return pass, nil
	}
	done := tenantBootstrapDone(cl)
	bootstrapping := res.Phase == readyPhase && p.opts.OnReady != nil && !done
	status := statusOf(cl, res)
	if done {
		meta.SetStatusCondition(&status.Conditions, tenantBootstrapped(cl))
	}
	if bootstrapping {
		status.Phase = v1alpha1.ClusterInitializing
	}
	if err := p.opts.Registry.WriteStatus(ctx, ref, status); err != nil {
		pass.Err = err
		return pass, err
	}
	pass.Wrote = true

	if err := p.addressMount(ctx, ref, res.URL); err != nil {
		pass.Err = err
		return pass, err
	}

	if res.RemoveFinalizer {
		if err := p.opts.Registry.RemoveFinalizer(ctx, ref); err != nil {
			pass.Err = err
			return pass, err
		}
		pass.Unfinalized = true
	}

	if bootstrapping {
		after := cl.DeepCopy()
		after.Status = statusOf(cl, res)
		if err := p.opts.OnReady(ctx, ref, after); err != nil {
			pass.Err = err
			return pass, err
		}
		meta.SetStatusCondition(&after.Status.Conditions, tenantBootstrapped(cl))
		if err := p.opts.Registry.WriteStatus(ctx, ref, after.Status); err != nil {
			pass.Err = err
			return pass, err
		}
	}
	if !bootstrapping && done && res.Phase == readyPhase &&
		p.opts.InstallTenant != nil && p.opts.Tenant != nil && p.tenantAddOnsDue(ref) {
		if err := p.refreshTenantAddOns(ctx, ref, cl); err != nil {
			pass.Err = err
			return pass, err
		}
	}
	return pass, nil
}

func (p *Provider) tenantAddOnsDue(ref Ref) bool {
	key, err := ref.Key()
	if err != nil {
		return false
	}
	now := p.opts.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.addOns[key]; ok && now.Sub(last) < p.opts.AddOnInterval {
		return false
	}
	if p.addOns == nil {
		p.addOns = map[string]time.Time{}
	}
	p.addOns[key] = now
	return true
}

func (p *Provider) refreshTenantAddOns(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) error {
	key, err := ref.Key()
	if err != nil {
		return err
	}
	cfg, err := p.opts.Tenant(ctx, ref, cl)
	if err != nil {
		return fmt.Errorf("clusterprovider: no tenant credential for the add-on refresh of %s: %w", ref.Name, err)
	}
	if cfg == nil {
		return fmt.Errorf("clusterprovider: no tenant credential for the add-on refresh of %s", ref.Name)
	}
	if err := p.opts.InstallTenant(ctx, key, cfg); err != nil {
		return fmt.Errorf("clusterprovider: refreshing the tenant add-ons of %s, which reissues the "+
			"client certificate the External Secrets store dials the vault with before it expires: %w",
			ref.Name, err)
	}
	p.opts.Log.Info("tenant add-ons refreshed", "cluster", ref.Name, "key", key)
	return nil
}

func (p *Provider) addressMount(ctx context.Context, ref Ref, url string) error {
	if url == "" || p.opts.MountChild == nil {
		return nil
	}
	if err := p.opts.MountChild.EnsureMountChild(ctx, ref.Home, ref.Name, url); err != nil {
		return fmt.Errorf("clusterprovider: the mount child at %s: %w", ref.MountPath(), err)
	}
	p.opts.Log.Debug("cluster provider: the mount child addresses the cluster's apiserver",
		"cluster", ref.Name, "home", ref.Home, "url", url)
	return nil
}

func tenantBootstrapped(cl *v1alpha1.Cluster) metav1.Condition {
	return metav1.Condition{
		Type:               conditionTenantBootstrapped,
		Status:             metav1.ConditionTrue,
		Reason:             reasonTenantBootstrapped,
		Message:            fmt.Sprintf("cluster uid %s generation %d bootstrapped", cl.UID, cl.Generation),
		ObservedGeneration: cl.Generation,
	}
}

func tenantBootstrapDone(cl *v1alpha1.Cluster) bool {
	want := tenantBootstrapped(cl)
	for i := range cl.Status.Conditions {
		held := cl.Status.Conditions[i]
		if held.Type != want.Type || held.Status != want.Status {
			continue
		}
		if held.ObservedGeneration == want.ObservedGeneration && held.Message == want.Message {
			return true
		}
	}
	return false
}

func (p *Provider) reportNodeExpiry(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) error {
	if p.opts.NodeExpirySource == nil {
		return nil
	}
	nodes, err := p.opts.Registry.Nodes(ctx, ref)
	if err != nil {
		return err
	}
	expiry, reported := p.opts.NodeExpirySource(cl, nodes)
	if !reported {
		cl.Status.NodeExpiry = nil
		return nil
	}
	cl.Status.NodeExpiry = &expiry
	return nil
}

func (p *Provider) Observe(ctx context.Context, ref Ref) (materialiser.Observed, error) {
	cl, err := p.opts.Registry.Read(ctx, ref)
	if err != nil {
		return materialiser.Observed{}, err
	}
	return p.observe(ctx, ref, cl)
}

func (p *Provider) observe(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) (materialiser.Observed, error) {
	key, err := ref.Key()
	if err != nil {
		return materialiser.Observed{}, err
	}
	o := materialiser.Observed{
		Cluster: *cl,
		Path:    p.opts.Registry.TenantPath(ctx, ref),
		Now:     p.opts.Now(),
	}
	o.Cluster.Name = key
	if cl.Status.URL != "" {
		_, o.Reachable = p.opts.Probe(ctx, cl.Status.URL)
	}
	state, err := p.opts.Registry.IdentityState(ctx, ref)
	if err != nil {
		return o, err
	}
	o.IdentityState = state

	if state == v1alpha1.IdentityRevocationPending {
		return o, errors.New(
			"clusterprovider: the account's identity is RevocationPending and whether its " +
				"ServiceAccount still exists is only observable through the tenant cluster's " +
				"credential, which this build does not have; refusing to claim Ready over a " +
				"deny that may not have landed")
	}
	return o, nil
}

func (p *Provider) Run(ctx context.Context) error {
	for {
		refs, err := p.opts.Registry.Refs(ctx)
		if err != nil {
			return err
		}
		p.reclaim(refs)
		for _, ref := range refs {
			pass, err := p.Reconcile(ctx, ref)
			if err != nil {
				p.opts.Log.Error("reconcile failed", "cluster", ref.Name, "home", ref.Home, "err", err)
				continue
			}
			p.opts.Log.Info("reconciled",
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
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.opts.Interval):
		}
	}
}

func (p *Provider) Reclaim(refs []Ref) { p.reclaim(refs) }

func (p *Provider) reclaim(refs []Ref) {
	if p.opts.Ports == nil || len(refs) == 0 {
		return
	}
	known := make(map[string]bool, len(refs))
	for _, ref := range refs {
		key, err := ref.Key()
		if err != nil {
			p.opts.Log.Error("portallocator: a Cluster CR has no derivable key, so its leases "+
				"cannot be told from leases whose CR is gone", "cluster", ref.Name, "home", ref.Home,
				"err", err.Error())
			return
		}
		known[key] = true
	}
	freed, err := p.opts.Ports.Reclaim(known)
	if err != nil {
		p.opts.Log.Error("portallocator: the reclaim pass failed; leases with no Cluster CR are still held",
			"err", err.Error())
		return
	}
	if len(freed) == 0 {
		return
	}
	held := make([]string, 0, len(freed))
	for _, l := range freed {
		held = append(held, fmt.Sprintf("%s/%s:%d", l.Cluster, l.Purpose, l.Port))
	}
	p.opts.Log.Info("portallocator: reclaimed leases whose Cluster CR is absent",
		"refs", len(refs), "freed", strings.Join(held, " "))
}

func carryStatus(cl *v1alpha1.Cluster) materialiser.Result {
	return materialiser.Result{
		Phase:      cl.Status.Phase,
		URL:        cl.Status.URL,
		Instance:   cl.Status.Instance,
		Conditions: cl.Status.Conditions,
	}
}

func statusOf(cl *v1alpha1.Cluster, res materialiser.Result) v1alpha1.ClusterStatus {
	return v1alpha1.ClusterStatus{
		Phase:      res.Phase,
		URL:        res.URL,
		Instance:   res.Instance,
		NodeExpiry: cl.Status.NodeExpiry,
		Conditions: res.Conditions,
	}
}

type clusterStatusPatch struct {
	Phase      v1alpha1.ClusterPhase `json:"phase,omitempty"`
	URL        string                `json:"URL,omitempty"`
	Instance   string                `json:"instance,omitempty"`
	NodeExpiry *int64                `json:"nodeExpiry"`
	Conditions []metav1.Condition    `json:"conditions,omitempty"`
}

func statusPatch(st v1alpha1.ClusterStatus) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"status": clusterStatusPatch{
		Phase:      st.Phase,
		URL:        st.URL,
		Instance:   st.Instance,
		NodeExpiry: st.NodeExpiry,
		Conditions: st.Conditions,
	}})
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: status patch: %w", err)
	}
	return body, nil
}

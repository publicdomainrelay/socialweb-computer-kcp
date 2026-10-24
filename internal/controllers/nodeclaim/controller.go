package nodeclaim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

const (
	Name = "nodeclaim"

	Export = "clusters"

	ContractExport = "computecontracts"

	ExposureExport = "exposures"

	homeIndexRefresh = 2 * time.Second

	lookupTimeout = 30 * time.Second

	resyncTimeout = 2 * time.Minute
)

var _ contract.Binder = (*Controller)(nil)

type Options struct {
	Bus *bus.Bus

	Log *slog.Logger
}

type known struct {
	workspace string

	links []string

	cluster string

	infra string

	entry string

	contract string
}

type Controller struct {
	bus *bus.Bus

	log *slog.Logger

	events chan ctlruntime.Request

	resyncs chan struct{}

	mu sync.Mutex

	binding *contract.Binding

	store *kcpstore.Store

	clusters map[string]string

	claims map[ctlruntime.Request]known

	byContract map[string]ctlruntime.Request

	infraMu sync.Mutex

	waiting map[ctlruntime.Request]bool

	homes map[string]bool

	homesAt time.Time
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, log: log.With("controller", Name), events: make(chan ctlruntime.Request, 64), resyncs: make(chan struct{}, 1),
		clusters: map[string]string{}, claims: map[ctlruntime.Request]known{}, byContract: map[string]ctlruntime.Request{},
		waiting: map[ctlruntime.Request]bool{}}
}

func Register(m *ctlruntime.Manager, c *Controller, cfg *rest.Config) error {
	if c == nil {
		return errors.New("nodeclaim: a controller is required")
	}
	if c.bus == nil {
		return errors.New("nodeclaim: the bus is required")
	}
	if cfg == nil || cfg.Host == "" {
		return errors.New("nodeclaim: a kcp rest config is required")
	}
	store, err := kcpstore.New(kcpstore.Options{Host: cfg.Host, RestConfig: cfg})
	if err != nil {
		return fmt.Errorf("nodeclaim: the kcp store: %w", err)
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return err
	}
	c.mu.Lock()
	c.store = store
	c.mu.Unlock()
	if err := ctlruntime.For[*v1alpha1.NodeClaim](m, Name, c,
		ctlruntime.Export(Export),
		ctlruntime.FieldManager(contract.FieldManager),
		ctlruntime.SharedStatus(),
		ctlruntime.Finalizer(v1alpha1.FinalizerNodeClaim),
		ctlruntime.InWorkspacesContext(c.inScope),
		ctlruntime.Gone(c.gone),
		ctlruntime.Events(c.events),
		ctlruntime.WatchesIn(ContractExport, c.contractClaims),
		ctlruntime.WatchesIn(ContractExport, c.infraClaims),
		ctlruntime.Watches(c.clusterClaims),
		ctlruntime.Watches(c.linkClaims),
		ctlruntime.Watches(joinedClaims),
		ctlruntime.WatchesIn(ExposureExport, c.exposureClaims),
	); err != nil {
		return fmt.Errorf("nodeclaim: registering the NodeClaim controller: %w", err)
	}
	if err := m.Go(Name+": resync", c.resyncLoop); err != nil {
		return fmt.Errorf("nodeclaim: the resync loop: %w", err)
	}
	c.wake()
	return nil
}

func (c *Controller) wake() {
	select {
	case c.resyncs <- struct{}{}:
	default:
	}
}

func (c *Controller) resyncLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.resyncs:
		}
		if b, _ := c.current(); b != nil {
			c.resync(ctx, *b)
		}
	}
}

func (c *Controller) Bind(b contract.Binding) {
	if b.Mapper == nil {
		return
	}
	b.Homes = append([]string(nil), b.Homes...)
	c.mu.Lock()
	c.binding = &b
	c.homes, c.homesAt = nil, time.Time{}
	c.mu.Unlock()
	c.log.Info("node_claim_bound", "owner", b.Owner.Workspace+"/"+b.Owner.Name, "homes", b.Homes,
		"indexWorkspace", b.IndexWorkspace, "indexNamespace", b.IndexNamespace, "contractWorkspace", b.ContractWorkspace)
	c.wake()
}

func (c *Controller) Unbind(owner commonv1.ObjectRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.binding != nil && c.binding.Owner == owner {
		c.binding = nil
		c.homes, c.homesAt = nil, time.Time{}
		c.log.Info("node_claim_unbound", "owner", owner.Workspace+"/"+owner.Name)
	}
}

func (c *Controller) current() (*contract.Binding, *kcpstore.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.binding, c.store
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.NodeClaim) (ctlruntime.Result, error) {
	b, store := c.current()
	if b == nil || store == nil {
		return ctlruntime.Result{Waiting: true}, nil
	}
	claim := contract.Claim{Workspace: req.Workspace, Object: *obj}
	at := entryOf(b, claim)
	openBao, apiserver := b.Mapper.LinkNames(claim)
	c.learn(req, known{workspace: req.Workspace, links: []string{openBao, apiserver}, cluster: obj.Spec.ClusterRef.Name,
		infra: at.Name, entry: at.Contract, contract: at.ComputeContract})
	cc, err := readContract(ctx, store, at)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	alive, err := clusterAlive(ctx, store, clusterRef(claim))
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !alive {
		if err := c.deleteInfra(ctx, store, at.Workspace, at.Name, "the cluster is deleting"); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if !obj.DeletionTimestamp.IsZero() {
		return c.settle(ctx, req, store, b, &claim, at, cc, alive)
	}
	if cc == nil {
		if !alive {
			c.wait(req, false)
			return ctlruntime.Result{}, nil
		}
		return c.ensure(ctx, req, store, b, &claim, at)
	}
	c.wait(req, false)
	if alive {
		if err := c.declare(ctx, store, claim, at, nil); err != nil && !errors.Is(err, errInfraDeleting) {
			return ctlruntime.Result{}, err
		}
	}
	next := b.Mapper.Status(claim, *cc)
	if next == nil {
		if claim.Object.Status.Infra != nil {
			return ctlruntime.Result{}, nil
		}
		same := statusCopy(claim.Object.Status)
		next = &same
	}
	if err := c.status(ctx, req, &claim.Object, at, *next); err != nil {
		return ctlruntime.Result{}, err
	}
	c.log.Info("node_claim_mapped", "claim", claim.Key(), "infra", at.Name, "entry", at.Contract, "contract", at.ComputeContract, "phase", next.Phase)
	return ctlruntime.Result{}, nil
}

func (c *Controller) ensure(ctx context.Context, req ctlruntime.Request, store *kcpstore.Store, b *contract.Binding, claim *contract.Claim,
	at v1alpha1.NodeClaimInfra) (ctlruntime.Result, error) {
	obj := &claim.Object
	ensure, adopt := b.Mapper.Wants(*claim)
	if !ensure {
		c.wait(req, false)
		return ctlruntime.Result{}, c.waitable(c.declare(ctx, store, *claim, at, nil))
	}
	bases, ok := b.Mapper.Bases(ctx, adopt)
	if !ok {
		c.wait(req, true)
		staged := c.entry(b, *claim, at, adopt, contract.Bases{})
		return ctlruntime.Result{}, c.waitable(c.declare(ctx, store, *claim, at, &staged))
	}
	c.wait(req, false)
	key := claim.Key()
	if !adopt {
		data, err := b.Mapper.Bundle(*claim, c.links(ctx, req, b, *claim))
		if err == nil {
			secrets := b.Mapper.BundleRef(at.ComputeContract)
			var kv vault.KV
			if kv, err = c.kv(ctx, b, claim.Ref(), secrets); err != nil {
				return ctlruntime.Result{}, err
			}
			err = kv.Write(ctx, secrets.Mount, secrets.Path, data)
		}
		if err != nil {
			c.log.Error("node_claim_bundle_failed", "claim", key, "err", err)
			return ctlruntime.Result{}, c.status(ctx, req, obj, at, b.Mapper.Failed(obj.Status, err))
		}
	}
	if err := c.status(ctx, req, obj, at, b.Mapper.Requested(obj.Status, adopt)); err != nil {
		return ctlruntime.Result{}, err
	}
	requested := c.entry(b, *claim, at, adopt, bases)
	if err := c.declare(ctx, store, *claim, at, &requested); err != nil {
		if errors.Is(err, errInfraDeleting) {
			c.log.Info("node_claim_infra_deleting", "claim", key, "infra", at.Name)
			return ctlruntime.Result{}, nil
		}
		c.log.Error("node_claim_entry_failed", "claim", key, "infra", at.Name, "entry", at.Contract, "err", err)
		return ctlruntime.Result{}, err
	}
	c.log.Info("node_claim_entry_requested", "claim", key, "infra", at.Name, "entry", at.Contract, "contract", at.ComputeContract,
		"adopt", adopt, "workspace", at.Workspace)
	return ctlruntime.Result{}, nil
}

func (c *Controller) waitable(err error) error {
	if errors.Is(err, errInfraDeleting) {
		return nil
	}
	return err
}

func (c *Controller) settle(ctx context.Context, req ctlruntime.Request, store *kcpstore.Store, b *contract.Binding, claim *contract.Claim,
	at v1alpha1.NodeClaimInfra, cc *v1alpha1.ComputeContract, alive bool) (ctlruntime.Result, error) {
	obj := &claim.Object
	c.wait(req, false)
	if !controllerutil.ContainsFinalizer(obj, v1alpha1.FinalizerNodeClaim) {
		return ctlruntime.Result{}, nil
	}
	key := claim.Key()
	if alive {
		if err := c.declare(ctx, store, *claim, at, nil); c.waitable(err) != nil {
			return ctlruntime.Result{Waiting: true}, err
		}
	}
	s := b.Mapper.Settle(*claim, cc)
	if s.Status != nil {
		if err := c.status(ctx, req, obj, at, *s.Status); err != nil {
			c.log.Warn("node_claim_status_write_failed", "claim", key, "err", err)
		}
	}
	if !s.Done {
		return ctlruntime.Result{Waiting: true}, nil
	}
	if s.RemoveBundle {
		if err := c.removeBundle(ctx, b, claim.Ref(), at.ComputeContract); err != nil {
			c.log.Warn("node_claim_bundle_remove_failed", "claim", key, "err", err)
		}
	}
	c.log.Info("node_claim_released", "claim", key, "infra", at.Name, "entry", at.Contract, "contract", at.ComputeContract, "phase", phaseOf(s.Status))
	return ctlruntime.Result{}, nil
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	k, ok := c.forget(req)
	if !ok || k.infra == "" {
		return
	}
	b, store := c.current()
	if b == nil || store == nil {
		return
	}
	if err := c.dropEntry(ctx, store, b.ContractWorkspace, k.infra, k.entry); err != nil {
		c.log.Warn("node_claim_entry_drop_failed", "claim", req.Workspace+"/"+req.Name, "infra", k.infra, "entry", k.entry, "err", err)
	}
}

func (c *Controller) status(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.NodeClaim, at v1alpha1.NodeClaimInfra,
	next v1alpha1.NodeClaimStatus) error {
	next.Infra = &at
	if reflect.DeepEqual(obj.Status, next) {
		return nil
	}
	own := next
	own.JoinedAt = ""
	if err := req.ApplyStatus(ctx, &v1alpha1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: obj.Name}, Status: own}); err != nil {
		return err
	}
	obj.Status = next
	return nil
}

func (c *Controller) links(ctx context.Context, req ctlruntime.Request, b *contract.Binding, claim contract.Claim) contract.Links {
	openBao, apiserver := b.Mapper.LinkNames(claim)
	return contract.Links{OpenBao: readLink(ctx, req, openBao), Apiserver: readLink(ctx, req, apiserver)}
}

func readLink(ctx context.Context, req ctlruntime.Request, name string) contract.Link {
	var link v1alpha1.ClusterLink
	err := req.Get(ctx, "", name, &link)
	switch {
	case apierrors.IsNotFound(err):
		return contract.Link{Name: name}
	case err != nil:
		return contract.Link{Name: name, Err: err}
	}
	return contract.Link{Name: name, Status: &link.Status}
}

func (c *Controller) kv(ctx context.Context, b *contract.Binding, from commonv1.ObjectRef, secrets commonv1.SecretRef) (vault.KV, error) {
	scope := strings.Trim(secrets.Mount, "/") + "/" + path.Dir(strings.Trim(secrets.Path, "/"))
	kv, err := bus.Resolve[vault.KV](ctx, c.bus, from, b.Vault, bus.Capability{Name: vault.CapabilityWrite, Scope: []string{scope}})
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: the vault handle for %s: %w", refKey(&from), err)
	}
	return kv, nil
}

func (c *Controller) removeBundle(ctx context.Context, b *contract.Binding, from commonv1.ObjectRef, name string) error {
	secrets := b.Mapper.BundleRef(name)
	kv, err := c.kv(ctx, b, from, secrets)
	if err != nil {
		return err
	}
	return kv.Delete(ctx, secrets.Mount, secrets.Path)
}

func (c *Controller) inScope(ctx context.Context, workspace string) bool {
	c.mu.Lock()
	b, store := c.binding, c.store
	if b == nil || store == nil {
		c.mu.Unlock()
		return false
	}
	if c.homes[workspace] || time.Since(c.homesAt) < homeIndexRefresh {
		in := c.homes[workspace]
		c.mu.Unlock()
		return in
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	homes := c.listHomes(ctx, store, *b)
	set := make(map[string]bool, len(homes))
	for _, home := range homes {
		set[home] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.binding == b {
		c.homes, c.homesAt = set, time.Now()
	}
	return set[workspace]
}

func (c *Controller) listHomes(ctx context.Context, store *kcpstore.Store, b contract.Binding) []string {
	items, err := kcpstore.Of[corev1.ConfigMap](store, kcpgvr.ConfigMaps).ListIn(ctx, b.IndexWorkspace, b.IndexNamespace)
	if err != nil {
		c.log.Warn("node_claim_home_index_unreadable", "workspace", b.IndexWorkspace, "namespace", b.IndexNamespace, "err", err)
		return append([]string(nil), b.Homes...)
	}
	seen := map[string]bool{}
	homes := []string{}
	for i := range items {
		cm := &items[i]
		if cm.Labels[kcpclient.NodeClaimHomeLabel] != "true" {
			continue
		}
		home := cm.Data[kcpclient.NodeClaimHomeKey]
		if home == "" || seen[home] {
			continue
		}
		seen[home] = true
		homes = append(homes, home)
	}
	return homes
}

func (c *Controller) learn(req ctlruntime.Request, k known) {
	key := ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.Workspace != "" && req.Cluster != "" {
		c.clusters[req.Workspace] = req.Cluster
	}
	if old, ok := c.claims[key]; ok && old.contract != k.contract {
		delete(c.byContract, old.contract)
	}
	c.claims[key] = k
	if k.contract != "" {
		c.byContract[k.contract] = key
	}
}

func (c *Controller) forget(req ctlruntime.Request) (known, bool) {
	key := ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.claims[key]
	delete(c.claims, key)
	delete(c.waiting, key)
	if ok && c.byContract[k.contract] == key {
		delete(c.byContract, k.contract)
	}
	return k, ok
}

func (c *Controller) wait(req ctlruntime.Request, waiting bool) {
	key := ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if waiting {
		c.waiting[key] = true
		return
	}
	delete(c.waiting, key)
}

func (c *Controller) contractClaims(_ context.Context, src ctlruntime.Request, obj *v1alpha1.ComputeContract) []ctlruntime.Request {
	b, _ := c.current()
	if b == nil || src.Workspace != b.ContractWorkspace {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.byContract[obj.Name]; ok {
		return []ctlruntime.Request{key}
	}
	return nil
}

func (c *Controller) infraClaims(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Infra) []ctlruntime.Request {
	b, _ := c.current()
	if b == nil || src.Workspace != b.ContractWorkspace {
		return nil
	}
	return c.claimsWhere(func(_ ctlruntime.Request, k known) bool { return k.infra == obj.Name })
}

func (c *Controller) clusterClaims(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Cluster) []ctlruntime.Request {
	return c.claimsWhere(func(key ctlruntime.Request, k known) bool { return key.Cluster == src.Cluster && k.cluster == obj.Name })
}

func (c *Controller) claimsWhere(match func(ctlruntime.Request, known) bool) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ctlruntime.Request
	for key, k := range c.claims {
		if match(key, k) {
			out = append(out, key)
		}
	}
	return out
}

func (c *Controller) linkClaims(_ context.Context, src ctlruntime.Request, obj *v1alpha1.ClusterLink) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ctlruntime.Request
	for key, k := range c.claims {
		if key.Cluster != src.Cluster {
			continue
		}
		for _, link := range k.links {
			if link == obj.Name {
				out = append(out, key)
				break
			}
		}
	}
	return out
}

func joinedClaims(_ context.Context, src ctlruntime.Request, obj *v1alpha1.NodeClaim) []ctlruntime.Request {
	if obj.Status.Phase != v1alpha1.NodeClaimJoining || obj.Status.JoinedAt == "" {
		return nil
	}
	return []ctlruntime.Request{{Namespace: src.Namespace, Name: src.Name}}
}

func (c *Controller) exposureClaims(_ context.Context, src ctlruntime.Request, _ *exposurev1alpha1.Exposure) []ctlruntime.Request {
	b, _ := c.current()
	if b == nil || src.Workspace != b.Workspace {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.waiting))
	for key := range c.waiting {
		out = append(out, key)
	}
	return out
}

func (c *Controller) clusterOf(ctx context.Context, workspace string) (string, error) {
	c.mu.Lock()
	cluster, ok := c.clusters[workspace]
	store := c.store
	c.mu.Unlock()
	if ok {
		return cluster, nil
	}
	if store == nil {
		return "", errors.New("nodeclaim: no kcp store yet")
	}
	i := strings.LastIndex(workspace, ":")
	if i <= 0 || i == len(workspace)-1 {
		return "", fmt.Errorf("nodeclaim: the workspace %q has no parent", workspace)
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	ws, err := kcpstore.OfClusterScoped[kcpv1alpha1.Workspace](store, kcpgvr.Workspaces).Get(ctx, ref.New(workspace[:i], "", workspace[i+1:]))
	if err != nil {
		return "", err
	}
	if ws.Spec.Cluster == "" {
		return "", fmt.Errorf("nodeclaim: the workspace %s has no logical cluster yet", workspace)
	}
	c.mu.Lock()
	c.clusters[workspace] = ws.Spec.Cluster
	c.mu.Unlock()
	return ws.Spec.Cluster, nil
}

func (c *Controller) resync(ctx context.Context, b contract.Binding) {
	ctx, cancel := context.WithTimeout(ctx, resyncTimeout)
	defer cancel()
	_, store := c.current()
	if store == nil {
		return
	}
	var out []ctlruntime.Request
	claims := kcpstore.OfClusterScoped[v1alpha1.NodeClaim](store, v1alpha1.NodeClaimGVR)
	for _, home := range c.listHomes(ctx, store, b) {
		cluster, err := c.clusterOf(ctx, home)
		if err != nil {
			c.log.Warn("node_claim_workspace_unresolved", "workspace", home, "err", err)
			continue
		}
		items, err := claims.List(ctx, home)
		if err != nil {
			if !kcpstore.IsNotFound(err) {
				c.log.Warn("node_claim_home_unlisted", "workspace", home, "err", err)
			}
			continue
		}
		for i := range items {
			out = append(out, ctlruntime.Request{Cluster: cluster, Name: items[i].Name})
		}
	}
	c.sweep(ctx, store, b)
	for _, r := range out {
		select {
		case c.events <- r:
		case <-ctx.Done():
			return
		}
	}
}

func refKey(r *commonv1.ObjectRef) string {
	if r == nil {
		return ""
	}
	return r.Workspace + "/" + r.Name
}

func phaseOf(status *v1alpha1.NodeClaimStatus) v1alpha1.NodeClaimPhase {
	if status == nil {
		return ""
	}
	return status.Phase
}

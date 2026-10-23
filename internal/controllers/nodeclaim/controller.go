package nodeclaim

import (
	"context"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
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

var (
	configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	workspaceGVR = kcpv1alpha1.SchemeGroupVersion.WithResource("workspaces")
)

var _ contract.Binder = (*Controller)(nil)

type Options struct {
	Bus *bus.Bus

	Log *slog.Logger
}

type known struct {
	workspace string

	links []string
}

type Controller struct {
	bus *bus.Bus

	log *slog.Logger

	events chan ctlruntime.Request

	mu sync.Mutex

	binding *contract.Binding

	store *kcpstore.Store

	clusters map[string]string

	claims map[ctlruntime.Request]known

	waiting map[ctlruntime.Request]bool

	homes map[string]bool

	homesAt time.Time
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, log: log.With("controller", Name), events: make(chan ctlruntime.Request, 64),
		clusters: map[string]string{}, claims: map[ctlruntime.Request]known{}, waiting: map[ctlruntime.Request]bool{}}
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
		ctlruntime.Finalizer(v1alpha1.FinalizerNodeClaim),
		ctlruntime.InWorkspaces(c.inScope),
		ctlruntime.Gone(c.gone),
		ctlruntime.Events(c.events),
		ctlruntime.WatchesIn(ContractExport, c.contractClaims),
		ctlruntime.Watches(c.linkClaims),
		ctlruntime.WatchesIn(ExposureExport, c.exposureClaims),
	); err != nil {
		return fmt.Errorf("nodeclaim: registering the NodeClaim controller: %w", err)
	}
	c.mu.Lock()
	b := c.binding
	c.mu.Unlock()
	if b != nil {
		go c.resync(*b)
	}
	return nil
}

func (c *Controller) Bind(b contract.Binding) {
	if b.Mapper == nil {
		return
	}
	b.Homes = append([]string(nil), b.Homes...)
	c.mu.Lock()
	c.binding = &b
	c.homes, c.homesAt = nil, time.Time{}
	store := c.store
	c.mu.Unlock()
	c.log.Info("node_claim_bound", "owner", b.Owner.Workspace+"/"+b.Owner.Name, "homes", b.Homes,
		"indexWorkspace", b.IndexWorkspace, "indexNamespace", b.IndexNamespace, "contractWorkspace", b.ContractWorkspace)
	if store != nil {
		go c.resync(b)
	}
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
	openBao, apiserver := b.Mapper.LinkNames(claim)
	c.learn(req, known{workspace: req.Workspace, links: []string{openBao, apiserver}})
	name := b.Mapper.ContractName(claim)
	contracts, err := c.contracts(ctx, store, b)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	var own *v1alpha1.ComputeContract
	for i := range contracts {
		cc := &contracts[i]
		if cc.Name == name {
			own = cc
			continue
		}
		if refersTo(cc, claim) {
			c.orphan(ctx, store, b, &claim, *cc)
		}
	}
	if !obj.DeletionTimestamp.IsZero() {
		return c.settle(ctx, req, store, b, claim, own)
	}
	if own == nil {
		return c.ensure(ctx, req, store, b, claim, name)
	}
	c.wait(req, false)
	next := b.Mapper.Status(claim, *own)
	if next == nil {
		return ctlruntime.Result{}, nil
	}
	if err := c.status(ctx, req, obj, *next); err != nil {
		return ctlruntime.Result{}, err
	}
	c.log.Info("node_claim_mapped", "claim", claim.Key(), "contract", name, "phase", next.Phase)
	return ctlruntime.Result{}, nil
}

func (c *Controller) ensure(ctx context.Context, req ctlruntime.Request, store *kcpstore.Store, b *contract.Binding, claim contract.Claim, name string) (ctlruntime.Result, error) {
	obj := &claim.Object
	ensure, adopt := b.Mapper.Wants(claim)
	if !ensure {
		c.wait(req, false)
		return ctlruntime.Result{}, nil
	}
	bases, ok := b.Mapper.Bases(ctx, adopt)
	if !ok {
		c.wait(req, true)
		return ctlruntime.Result{}, nil
	}
	c.wait(req, false)
	key := claim.Key()
	if !adopt {
		data, err := b.Mapper.Bundle(claim, c.links(ctx, req, b, claim))
		if err != nil {
			c.log.Error("node_claim_contract_failed", "claim", key, "err", err)
			return ctlruntime.Result{}, c.status(ctx, req, obj, b.Mapper.Failed(obj.Status, err))
		}
		secrets := b.Mapper.BundleRef(name)
		kv, err := c.kv(ctx, b, claim.Ref(), secrets)
		if err != nil {
			return ctlruntime.Result{}, err
		}
		if err := kv.Write(ctx, secrets.Mount, secrets.Path, data); err != nil {
			c.log.Error("node_claim_contract_failed", "claim", key, "err", err)
			return ctlruntime.Result{}, c.status(ctx, req, obj, b.Mapper.Failed(obj.Status, err))
		}
	}
	cc := v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: b.Mapper.Contract(claim, name, adopt, bases)}
	if obj.UID != "" {
		cc.Labels = map[string]string{contract.UIDLabel: string(obj.UID)}
	}
	if err := applyContract(ctx, store, b.ContractWorkspace, cc); err != nil {
		c.log.Error("node_claim_contract_failed", "claim", key, "err", err)
		return ctlruntime.Result{}, c.status(ctx, req, obj, b.Mapper.Failed(obj.Status, err))
	}
	c.log.Info("node_claim_contract_created", "claim", key, "contract", name, "adopt", adopt, "workspace", b.ContractWorkspace)
	return ctlruntime.Result{}, c.status(ctx, req, obj, b.Mapper.Requested(obj.Status, adopt))
}

func (c *Controller) settle(ctx context.Context, req ctlruntime.Request, store *kcpstore.Store, b *contract.Binding, claim contract.Claim, own *v1alpha1.ComputeContract) (ctlruntime.Result, error) {
	obj := &claim.Object
	c.wait(req, false)
	if !controllerutil.ContainsFinalizer(obj, v1alpha1.FinalizerNodeClaim) {
		return ctlruntime.Result{}, nil
	}
	key := claim.Key()
	s := b.Mapper.Settle(claim, own)
	if s.Release && own != nil {
		if err := applyRelease(ctx, store, b.ContractWorkspace, *own); err != nil {
			return ctlruntime.Result{Waiting: true}, err
		}
		c.log.Info("node_claim_release_requested", "claim", key, "contract", own.Name)
		return ctlruntime.Result{Waiting: true}, nil
	}
	if s.Status != nil {
		if err := c.status(ctx, req, obj, *s.Status); err != nil {
			c.log.Warn("node_claim_status_write_failed", "claim", key, "err", err)
		}
	}
	if !s.Done {
		return ctlruntime.Result{Waiting: true}, nil
	}
	name := b.Mapper.ContractName(claim)
	if s.DeleteContract {
		if err := deleteContract(ctx, store, b.ContractWorkspace, name); err != nil {
			c.log.Warn("node_claim_contract_delete_failed", "claim", key, "err", err)
		}
	}
	if s.RemoveBundle {
		if err := c.removeBundle(ctx, b, claim.Ref(), name); err != nil {
			c.log.Warn("node_claim_bundle_remove_failed", "claim", key, "err", err)
		}
	}
	c.log.Info("node_claim_released", "claim", key, "contract", name, "phase", phaseOf(s.Status))
	return ctlruntime.Result{}, nil
}

func (c *Controller) orphan(ctx context.Context, store *kcpstore.Store, b *contract.Binding, claim *contract.Claim, cc v1alpha1.ComputeContract) {
	action, orphan := b.Mapper.Orphan(claim, cc)
	if !orphan {
		return
	}
	owner := refKey(cc.Spec.NodeClaimRef)
	switch {
	case action.Delete:
		if err := deleteContract(ctx, store, b.ContractWorkspace, cc.Name); err != nil {
			c.log.Error("node_claim_contract_delete_failed", "claim", owner, "err", err)
			return
		}
		if err := c.removeBundle(ctx, b, *cc.Spec.NodeClaimRef, cc.Name); err != nil {
			c.log.Error("node_claim_bundle_remove_failed", "claim", owner, "err", err)
		}
		c.log.Info("node_claim_orphan_contract_removed", "claim", owner, "contract", cc.Name, "phase", cc.Status.Phase)
	case action.Release:
		if err := applyRelease(ctx, store, b.ContractWorkspace, cc); err != nil {
			c.log.Error("node_claim_orphan_release_failed", "claim", owner, "contract", cc.Name, "err", err)
			return
		}
		c.log.Info("node_claim_orphan_release_requested", "claim", owner, "contract", cc.Name, "phase", cc.Status.Phase)
	}
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	c.forget(req)
	b, store := c.current()
	if b == nil || store == nil {
		return
	}
	contracts, err := c.contracts(ctx, store, b)
	if err != nil {
		c.log.Warn("node_claim_orphan_lookup_failed", "claim", req.Workspace+"/"+req.Name, "err", err)
		return
	}
	for _, cc := range contracts {
		if r := cc.Spec.NodeClaimRef; r != nil && r.Workspace == req.Workspace && r.Name == req.Name {
			c.orphan(ctx, store, b, nil, cc)
		}
	}
}

func (c *Controller) status(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.NodeClaim, next v1alpha1.NodeClaimStatus) error {
	if reflect.DeepEqual(obj.Status, next) {
		return nil
	}
	if err := req.ApplyStatus(ctx, &v1alpha1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: obj.Name}, Status: next}); err != nil {
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

func (c *Controller) contracts(ctx context.Context, store *kcpstore.Store, b *contract.Binding) ([]v1alpha1.ComputeContract, error) {
	items, err := kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, v1alpha1.ComputeContractGVR).List(ctx, b.ContractWorkspace)
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: listing the ComputeContracts of %s: %w", b.ContractWorkspace, err)
	}
	return items, nil
}

func refersTo(cc *v1alpha1.ComputeContract, claim contract.Claim) bool {
	r := cc.Spec.NodeClaimRef
	return r != nil && r.Workspace == claim.Workspace && r.Name == claim.Object.Name
}

func applyContract(ctx context.Context, store *kcpstore.Store, workspace string, cc v1alpha1.ComputeContract) error {
	meta := map[string]any{"name": cc.Name}
	if len(cc.Labels) > 0 {
		meta["labels"] = cc.Labels
	}
	body, err := json.Marshal(map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "ComputeContract",
		"metadata": meta, "spec": cc.Spec})
	if err != nil {
		return fmt.Errorf("nodeclaim: encoding ComputeContract %s: %w", cc.Name, err)
	}
	return kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, v1alpha1.ComputeContractGVR).
		Apply(ctx, ref.New(workspace, "", cc.Name), body, contract.FieldManager)
}

func applyRelease(ctx context.Context, store *kcpstore.Store, workspace string, cc v1alpha1.ComputeContract) error {
	next := v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: cc.Name, Labels: cc.Labels}}
	cc.Spec.DeepCopyInto(&next.Spec)
	next.Spec.Release = true
	return applyContract(ctx, store, workspace, next)
}

func deleteContract(ctx context.Context, store *kcpstore.Store, workspace, name string) error {
	return kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, v1alpha1.ComputeContractGVR).Delete(ctx, ref.New(workspace, "", name))
}

func (c *Controller) inScope(workspace string) bool {
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
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
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
	items, err := kcpstore.Of[corev1.ConfigMap](store, configMapGVR).ListIn(ctx, b.IndexWorkspace, b.IndexNamespace)
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
	c.claims[key] = k
}

func (c *Controller) forget(req ctlruntime.Request) {
	key := ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.claims, key)
	delete(c.waiting, key)
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

func (c *Controller) contractClaims(ctx context.Context, src ctlruntime.Request, obj *v1alpha1.ComputeContract) []ctlruntime.Request {
	r := obj.Spec.NodeClaimRef
	if r == nil || r.Name == "" || r.Workspace == "" || !strings.EqualFold(r.Kind, contract.Kind) {
		return nil
	}
	b, _ := c.current()
	if b == nil || src.Workspace != b.ContractWorkspace {
		return nil
	}
	cluster, err := c.clusterOf(ctx, r.Workspace)
	if err != nil {
		c.log.Warn("node_claim_workspace_unresolved", "claim", refKey(r), "contract", obj.Name, "err", err)
		return nil
	}
	return []ctlruntime.Request{{Cluster: cluster, Name: r.Name}}
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
	ws, err := kcpstore.OfClusterScoped[kcpv1alpha1.Workspace](store, workspaceGVR).Get(ctx, ref.New(workspace[:i], "", workspace[i+1:]))
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

func (c *Controller) resync(b contract.Binding) {
	ctx, cancel := context.WithTimeout(context.Background(), resyncTimeout)
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
	if contracts, err := c.contracts(ctx, store, &b); err == nil {
		for i := range contracts {
			r := contracts[i].Spec.NodeClaimRef
			if r == nil || r.Workspace == "" || r.Name == "" {
				continue
			}
			cluster, err := c.clusterOf(ctx, r.Workspace)
			if err != nil {
				c.log.Warn("node_claim_workspace_unresolved", "claim", refKey(r), "err", err)
				continue
			}
			out = append(out, ctlruntime.Request{Cluster: cluster, Name: r.Name})
		}
	} else {
		c.log.Warn("node_claim_contracts_unlisted", "workspace", b.ContractWorkspace, "err", err)
	}
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

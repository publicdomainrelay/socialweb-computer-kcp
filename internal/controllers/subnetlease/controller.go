package subnetlease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/lease/local"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

const (
	Name = "subnetlease"

	Export = "applications"

	ClusterExport = "clusters"

	ConditionReady = "Ready"
)

type Options struct {
	Subnets local.Subnets

	Changes <-chan struct{}

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	subnets local.Subnets

	scope func(workspace string) bool

	log *slog.Logger

	holders *local.Holders
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Subnets == nil {
		return nil, errors.New("subnetlease: a subnet allocator is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctl := &Controller{subnets: opts.Subnets, scope: opts.Scope, log: log.With("controller", Name), holders: local.NewHolders()}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone),
		ctlruntime.WatchesIn(ClusterExport, ctl.engage)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.Changes != nil {
		events, err := ctlruntime.Forward(m, Name+": changes", opts.Changes, ctl.changed)
		if err != nil {
			return nil, fmt.Errorf("subnetlease: forwarding changes: %w", err)
		}
		options = append(options, ctlruntime.Events(events))
	}
	if err := ctlruntime.For[*v1alpha1.SubnetLease](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("subnetlease: registering the SubnetLease controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.SubnetLease) (ctlruntime.Result, error) {
	target := local.Target{Cluster: req.Cluster, Name: req.Name}
	c.holders.Engage(req.Workspace, req.Cluster, "")
	ref := object.Spec.ClusterRef
	key := local.Key(ref.Workspace, ref.Name, object.Spec.Purpose, object.Name)
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, c.release(target, object.Status.HolderRef, key)
	}
	c.holders.Hold(target, key)
	status, allocErr := c.allocate(object, key)
	err := errors.Join(allocErr, c.apply(ctx, req, object, status), c.mirror(ctx, req))
	return ctlruntime.Result{}, err
}

func (c *Controller) allocate(object *v1alpha1.SubnetLease, key string) (v1alpha1.SubnetLeaseStatus, error) {
	gen := object.Generation
	status := *object.Status.DeepCopy()
	status.ObservedGeneration = gen
	if key == "" {
		status.Phase = v1alpha1.LeasePhasePending
		conds.Set(&status.Conditions, gen, ConditionReady, false, "InvalidSpec", "the SubnetLease names no workspace, cluster or purpose")
		return status, nil
	}
	if held := strings.TrimSpace(status.HolderRef); held != "" && held != key {
		if !c.holders.Held(held) {
			if err := c.subnets.Release(held); err != nil {
				return status, fmt.Errorf("subnetlease: releasing the superseded subnet lease %s of SubnetLease %s: %w", held, object.Name, err)
			}
		}
		status.HolderRef = ""
	}
	var want netip.Prefix
	if requested := strings.TrimSpace(object.Spec.Prefix); requested != "" {
		parsed, err := netip.ParsePrefix(requested)
		if err != nil {
			status.Phase = v1alpha1.LeasePhasePending
			conds.Set(&status.Conditions, gen, ConditionReady, false, "InvalidPrefix", err.Error())
			return status, nil
		}
		want = parsed.Masked()
	}
	granted, err := c.subnets.Allocate(subnetallocator.Request{Cluster: key})
	if err != nil {
		status.Phase = v1alpha1.LeasePhasePending
		conds.Set(&status.Conditions, gen, ConditionReady, false, "AllocateFailed", err.Error())
		return status, fmt.Errorf("subnetlease: allocating a subnet for SubnetLease %s: %w", object.Name, err)
	}
	if want.IsValid() && granted.Block.Masked() != want {
		if err := c.subnets.Release(key); err != nil {
			return status, fmt.Errorf("subnetlease: releasing the subnet granted instead of the requested prefix of SubnetLease %s: %w", object.Name, err)
		}
		status.Phase = v1alpha1.LeasePhasePending
		conds.Set(&status.Conditions, gen, ConditionReady, false, "RequestedPrefixUnavailable",
			fmt.Sprintf("the subnet allocator granted %s for %s; the SubnetLease asks for %s", granted.Block.Masked(), key, want))
		return status, nil
	}
	status.Phase = v1alpha1.LeasePhaseAllocated
	status.AllocatedAt = formatTime(granted.AllocatedAt)
	status.HolderRef = key
	conds.Set(&status.Conditions, gen, ConditionReady, true, "Allocated",
		fmt.Sprintf("the subnet allocator holds %s for %s", granted.Block.Masked(), key))
	return status, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.SubnetLease, status v1alpha1.SubnetLeaseStatus) error {
	if reflect.DeepEqual(object.Status, status) {
		return nil
	}
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	c.holders.Engage(req.Workspace, req.Cluster, "")
	if err := c.release(local.Target{Cluster: req.Cluster, Name: req.Name}); err != nil {
		c.log.Warn("releasing the subnet of a deleted SubnetLease", "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
	if err := c.mirror(ctx, req); err != nil {
		c.log.Warn("mirroring the subnet allocator", "workspace", req.Workspace, "err", err.Error())
	}
}

func (c *Controller) release(target local.Target, extra ...string) error {
	var errs []error
	for _, key := range c.holders.Drop(target, extra...) {
		if err := c.subnets.Release(key); err != nil {
			errs = append(errs, fmt.Errorf("subnetlease: releasing subnet lease %s of SubnetLease %s: %w", key, target.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) leases() map[string]netip.Prefix {
	out := map[string]netip.Prefix{}
	for _, l := range c.subnets.Leases() {
		if l.Block.IsValid() {
			out[strings.TrimSpace(l.Cluster)] = l.Block.Masked()
		}
	}
	return out
}

func keysOf(leases map[string]netip.Prefix) []string {
	out := make([]string, 0, len(leases))
	for k := range leases {
		out = append(out, k)
	}
	return out
}

func (c *Controller) mirror(ctx context.Context, req ctlruntime.Request) error {
	leases := c.leases()
	pending := c.holders.Pending(req.Workspace, keysOf(leases))
	if len(pending) == 0 {
		return nil
	}
	var list v1alpha1.SubnetLeaseList
	if err := req.List(ctx, &list); err != nil {
		return fmt.Errorf("subnetlease: listing the SubnetLeases of workspace %s: %w", req.Workspace, err)
	}
	present := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		present[local.Key(item.Spec.ClusterRef.Workspace, item.Spec.ClusterRef.Name, item.Spec.Purpose, item.Name)] = true
	}
	var errs []error
	for _, key := range pending {
		if present[key] {
			continue
		}
		home, _ := local.ParseKey(key)
		next := &v1alpha1.SubnetLease{
			ObjectMeta: metav1.ObjectMeta{Name: local.MirrorName(Name, key)},
			Spec: v1alpha1.SubnetLeaseSpec{
				ClusterRef: commonv1.ObjectRef{Kind: "Cluster", Name: home.Cluster, Workspace: home.Workspace},
				Purpose:    home.Purpose,
				Prefix:     leases[key].String(),
			},
		}
		if err := req.Apply(ctx, next); err != nil {
			errs = append(errs, fmt.Errorf("subnetlease: mirroring subnet lease %s into workspace %s: %w", key, req.Workspace, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) engage(_ context.Context, src ctlruntime.Request, cluster *v1alpha1.Cluster) []ctlruntime.Request {
	if c.scope != nil && !c.scope(src.Workspace) {
		return nil
	}
	c.holders.Engage(src.Workspace, src.Cluster, cluster.Name)
	var out []ctlruntime.Request
	for _, key := range c.holders.Pending(src.Workspace, keysOf(c.leases())) {
		out = append(out, ctlruntime.Request{Cluster: src.Cluster, Name: local.MirrorName(Name, key)})
	}
	return out
}

func (c *Controller) changed(struct{}) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, p := range c.holders.Placements(keysOf(c.leases())) {
		out = append(out, ctlruntime.Request{Cluster: p.Cluster, Name: local.MirrorName(Name, p.Key)})
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

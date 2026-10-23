package portlease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/lease/local"
)

const (
	Name = "portlease"

	Export = "applications"

	ClusterExport = "clusters"

	ConditionReady = "Ready"
)

type Options struct {
	Ports local.Ports

	Changes <-chan struct{}

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	ports local.Ports

	scope func(workspace string) bool

	log *slog.Logger

	holders *local.Holders

	events chan ctlruntime.Request
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Ports == nil {
		return nil, errors.New("portlease: a port registry is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctl := &Controller{ports: opts.Ports, scope: opts.Scope, log: log.With("controller", Name), holders: local.NewHolders()}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone),
		ctlruntime.WatchesIn(ClusterExport, ctl.engage)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.Changes != nil {
		ctl.events = make(chan ctlruntime.Request, 64)
		options = append(options, ctlruntime.Events(ctl.events))
	}
	if err := ctlruntime.For[*v1alpha1.PortLease](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("portlease: registering the PortLease controller: %w", err)
	}
	if opts.Changes != nil {
		go ctl.forward(opts.Changes)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.PortLease) (ctlruntime.Result, error) {
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

func (c *Controller) allocate(object *v1alpha1.PortLease, key string) (v1alpha1.PortLeaseStatus, error) {
	gen := object.Generation
	status := *object.Status.DeepCopy()
	status.ObservedGeneration = gen
	if key == "" {
		status.Phase = v1alpha1.LeasePhasePending
		conds.Set(&status.Conditions, gen, ConditionReady, false, "InvalidSpec", "the PortLease names no workspace, cluster or purpose")
		return status, nil
	}
	if held := strings.TrimSpace(status.HolderRef); held != "" && held != key {
		if !c.holders.Held(held) {
			if err := c.ports.Release(held); err != nil {
				return status, fmt.Errorf("portlease: releasing the superseded port lease %s of PortLease %s: %w", held, object.Name, err)
			}
		}
		status.HolderRef = ""
	}
	granted, err := c.ports.Allocate(key)
	if err != nil {
		status.Phase = v1alpha1.LeasePhasePending
		conds.Set(&status.Conditions, gen, ConditionReady, false, "AllocateFailed", err.Error())
		return status, fmt.Errorf("portlease: allocating a port for PortLease %s: %w", object.Name, err)
	}
	if object.Spec.Port > 0 && int(object.Spec.Port) != granted.Port {
		if err := c.ports.Release(key); err != nil {
			return status, fmt.Errorf("portlease: releasing the port granted instead of the requested port of PortLease %s: %w", object.Name, err)
		}
		status.Phase = v1alpha1.LeasePhasePending
		conds.Set(&status.Conditions, gen, ConditionReady, false, "RequestedPortUnavailable",
			fmt.Sprintf("the port registry granted port %d for %s; the PortLease asks for port %d", granted.Port, key, object.Spec.Port))
		return status, nil
	}
	status.Phase = v1alpha1.LeasePhaseAllocated
	status.AllocatedAt = formatTime(granted.AllocatedAt)
	status.HolderRef = key
	conds.Set(&status.Conditions, gen, ConditionReady, true, "Allocated",
		fmt.Sprintf("the port registry holds port %d on %s for %s", granted.Port, granted.Host, key))
	return status, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.PortLease, status v1alpha1.PortLeaseStatus) error {
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
		c.log.Warn("releasing the port of a deleted PortLease", "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
	if err := c.mirror(ctx, req); err != nil {
		c.log.Warn("mirroring the port registry", "workspace", req.Workspace, "err", err.Error())
	}
}

func (c *Controller) release(target local.Target, extra ...string) error {
	var errs []error
	for _, key := range c.holders.Drop(target, extra...) {
		if err := c.ports.Release(key); err != nil {
			errs = append(errs, fmt.Errorf("portlease: releasing port lease %s of PortLease %s: %w", key, target.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) leases() map[string]int32 {
	out := map[string]int32{}
	for _, l := range c.ports.Leases() {
		if l.Port > 0 && l.Port <= 65535 {
			out[strings.TrimSpace(l.Name)] = int32(l.Port)
		}
	}
	return out
}

func keysOf(leases map[string]int32) []string {
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
	var list v1alpha1.PortLeaseList
	if err := req.List(ctx, &list); err != nil {
		return fmt.Errorf("portlease: listing the PortLeases of workspace %s: %w", req.Workspace, err)
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
		next := &v1alpha1.PortLease{
			ObjectMeta: metav1.ObjectMeta{Name: local.MirrorName(Name, key)},
			Spec: v1alpha1.PortLeaseSpec{
				ClusterRef: commonv1.ObjectRef{Kind: "Cluster", Name: home.Cluster, Workspace: home.Workspace},
				Purpose:    home.Purpose,
				Port:       leases[key],
			},
		}
		if err := req.Apply(ctx, next); err != nil {
			errs = append(errs, fmt.Errorf("portlease: mirroring port lease %s into workspace %s: %w", key, req.Workspace, err))
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

func (c *Controller) forward(changes <-chan struct{}) {
	defer close(c.events)
	for range changes {
		for _, p := range c.holders.Placements(keysOf(c.leases())) {
			c.events <- ctlruntime.Request{Cluster: p.Cluster, Name: local.MirrorName(Name, p.Key)}
		}
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component/ports"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/lease/local"
)

const (
	Name = "cluster"

	Export = "clusters"

	LeaseExport = "applications"

	FrontHolder = "/front"
)

type Options struct {
	Ports local.Ports

	Front local.Front

	Changes <-chan struct{}

	Scope func(workspace string) bool
}

type Controller struct {
	ports local.Ports

	front local.Front

	mu sync.Mutex

	known map[ctlruntime.Request]bool

	events chan ctlruntime.Request
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Ports == nil {
		return nil, errors.New("cluster: a port registry is required")
	}
	if opts.Front == nil {
		return nil, errors.New("cluster: an apiserver front is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := &Controller{ports: opts.Ports, front: opts.Front, known: map[ctlruntime.Request]bool{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.SharedStatus(), ctlruntime.Gone(ctl.gone),
		ctlruntime.WatchesIn(LeaseExport, ctl.holderOf)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.Changes != nil {
		ctl.events = make(chan ctlruntime.Request, 16)
		options = append(options, ctlruntime.Events(ctl.events))
	}
	if err := ctlruntime.For[*v1alpha1.Cluster](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("cluster: registering the Cluster controller: %w", err)
	}
	if opts.Changes != nil {
		go ctl.forward(opts.Changes)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Cluster) (ctlruntime.Result, error) {
	self := ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.forget(self)
		return ctlruntime.Result{}, nil
	}
	c.remember(self)
	name := strings.TrimSpace(object.Name)
	registry := registryHeldPorts(c.ports.Leases(), req.Workspace, name)
	state, err := c.front.Status(ctx)
	if err != nil {
		var kept []v1alpha1.ClusterHeldPort
		for _, held := range object.Status.HeldPorts {
			if held.HolderRef == name+FrontHolder {
				kept = append(kept, held)
			}
		}
		applyErr := c.apply(ctx, req, object, object.Status.FrontAddress, mergeHeldPorts(nil, name, kept, registry))
		return ctlruntime.Result{}, errors.Join(fmt.Errorf("cluster: reading the apiserver front of cluster %s: %w", name, err), applyErr)
	}
	return ctlruntime.Result{}, c.apply(ctx, req, object, strings.TrimSpace(state.Address), mergeHeldPorts(state.Holds, name, registry))
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Cluster, address string, held []v1alpha1.ClusterHeldPort) error {
	if object.Status.FrontAddress == address && reflect.DeepEqual(object.Status.HeldPorts, held) {
		return nil
	}
	next := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: object.Name, Namespace: object.Namespace},
		Status: v1alpha1.ClusterStatus{FrontAddress: address, HeldPorts: held}}
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.forget(ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name})
}

func (c *Controller) remember(r ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known[r] = true
}

func (c *Controller) forget(r ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.known, r)
}

func (c *Controller) all() []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.known))
	for r := range c.known {
		out = append(out, r)
	}
	return out
}

func (c *Controller) holderOf(_ context.Context, src ctlruntime.Request, lease *v1alpha1.PortLease) []ctlruntime.Request {
	ref := lease.Spec.ClusterRef
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return nil
	}
	if ws := strings.TrimSpace(ref.Workspace); ws != "" && ws != src.Workspace {
		return nil
	}
	target := ctlruntime.Request{Cluster: src.Cluster, Name: name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.known[target] {
		return nil
	}
	return []ctlruntime.Request{target}
}

func (c *Controller) forward(changes <-chan struct{}) {
	defer close(c.events)
	for range changes {
		for _, r := range c.all() {
			c.events <- r
		}
	}
}

func mergeHeldPorts(front map[string]int, cluster string, extra ...[]v1alpha1.ClusterHeldPort) []v1alpha1.ClusterHeldPort {
	byPort := make(map[int32]v1alpha1.ClusterHeldPort, len(front))
	for purpose, port := range front {
		if port <= 0 || port > 65535 {
			continue
		}
		byPort[int32(port)] = v1alpha1.ClusterHeldPort{Port: int32(port), Purpose: purpose, HolderRef: cluster + FrontHolder}
	}
	for _, group := range extra {
		for _, held := range group {
			if held.Port <= 0 || held.Port > 65535 {
				continue
			}
			if _, ok := byPort[held.Port]; !ok {
				byPort[held.Port] = held
			}
		}
	}
	if len(byPort) == 0 {
		return nil
	}
	out := make([]v1alpha1.ClusterHeldPort, 0, len(byPort))
	for _, held := range byPort {
		out = append(out, held)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func registryHeldPorts(leases []ports.Lease, workspace, cluster string) []v1alpha1.ClusterHeldPort {
	out := make([]v1alpha1.ClusterHeldPort, 0, len(leases))
	for _, lease := range leases {
		if !local.HeldBy(lease.Name, workspace, cluster) || lease.Port <= 0 || lease.Port > 65535 {
			continue
		}
		purpose := lease.Name
		if i := strings.LastIndex(lease.Name, "/"); i >= 0 && i+1 < len(lease.Name) {
			purpose = lease.Name[i+1:]
		}
		out = append(out, v1alpha1.ClusterHeldPort{Port: int32(lease.Port), Purpose: purpose, HolderRef: lease.Name})
	}
	return out
}

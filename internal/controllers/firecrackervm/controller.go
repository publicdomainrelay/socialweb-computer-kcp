package firecrackervm

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/compute/firecracker"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	Name = "firecrackervm"

	Export = "computevms"

	Workers = 8
)

type Controller struct {
	p *firecracker.Provider

	host *computevm.Host

	placement *Placement

	log *slog.Logger

	events chan ctlruntime.Request

	life context.Context

	end context.CancelFunc

	mu sync.Mutex

	clusters map[string]string
}

func Register(m *ctlruntime.Manager, p *firecracker.Provider, host *computevm.Host, placement *Placement) (*Controller, error) {
	life, end := context.WithCancel(context.Background())
	ctl := &Controller{p: p, host: host, placement: placement, log: host.Log().With("controller", Name), events: make(chan ctlruntime.Request, 256),
		life: life, end: end, clusters: map[string]string{}}
	handle := p.Handle()
	if err := ctlruntime.For[*v1alpha1.FirecrackerVM](m, Name, ctl, ctlruntime.Export(Export),
		ctlruntime.Finalizer(v1alpha1.FinalizerFirecrackerVM), ctlruntime.Workers(Workers), ctlruntime.Events(ctl.events),
		ctlruntime.Only(func(vm *v1alpha1.FirecrackerVM) bool { return vm.Spec.Owner.Handle == handle })); err != nil {
		end()
		return nil, fmt.Errorf("firecrackervm: registering the FirecrackerVM controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) remember(name, cluster string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusters[name] = cluster
}

func (c *Controller) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.clusters, name)
}

func (c *Controller) clusterOf(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.clusters[name]
	return cl, ok
}

func (c *Controller) Run(ctx context.Context) error {
	defer close(c.events)
	defer c.end()
	sweep := time.NewTimer(c.host.Config().Grace())
	defer sweep.Stop()
	exits := c.p.VMMExits()
	for ctx.Err() == nil {
		names, err := c.p.Watch(ctx)
		var retry <-chan time.Time
		if err != nil {
			c.log.Warn("watching the VM units; the watch is opened again", "err", err.Error(), "after", computevm.RetryAfter.String())
			retry = time.After(computevm.RetryAfter)
		}
	events:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-sweep.C:
				c.sweep(ctx)
			case <-retry:
				break events
			case name := <-exits:
				if !c.enqueue(ctx, name) {
					return nil
				}
			case name, ok := <-names:
				if !ok {
					break events
				}
				if !c.enqueue(ctx, name) {
					return nil
				}
			}
		}
	}
	return nil
}

func (c *Controller) enqueue(ctx context.Context, name string) bool {
	cluster, known := c.clusterOf(name)
	if !known {
		return true
	}
	select {
	case c.events <- ctlruntime.Request{Cluster: cluster, Name: name}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Controller) sweep(ctx context.Context) {
	for _, l := range c.p.Listed(ctx) {
		if ok, err := c.placement.exists(ctx, l.Name); ok || err != nil {
			continue
		}
		c.log.Warn("a VM unit or work dir with no FirecrackerVM of this handle outlived the grace; collecting it", "name", l.Name,
			"unit", l.Unit, "loaded", l.Loaded, "state", string(l.Status.State), "workDir", l.WorkDir)
		if err := c.p.Collect(ctx, l); err != nil {
			c.log.Warn("collecting an orphan VM", "name", l.Name, "err", err.Error())
		}
	}
	c.placement.collectChildren(ctx, c.p.Live)
	c.mu.Lock()
	none := len(c.clusters) == 0
	c.mu.Unlock()
	if none {
		c.p.DropSlice(ctx)
	}
}

func (c *Controller) live(ctx context.Context, req ctlruntime.Request) ([]v1alpha1.FirecrackerVM, error) {
	var list v1alpha1.FirecrackerVMList
	if err := req.List(ctx, &list); err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for _, vm := range list.Items {
		if vm.Spec.Owner.Handle == c.p.Handle() && vm.DeletionTimestamp.IsZero() {
			out = append(out, vm)
		}
	}
	return out, nil
}

func (c *Controller) collectImages(ctx context.Context, req ctlruntime.Request) {
	vms, err := c.live(ctx, req)
	if err != nil {
		return
	}
	keep := map[string]string{}
	for _, vm := range vms {
		if img := vm.Status.Image; img != nil && img.Sha256 != "" {
			keep[img.Sha256] = "the FirecrackerVM " + vm.Name + " names it"
		}
	}
	c.p.CollectImages(ctx, keep)
}

func (c *Controller) write(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.FirecrackerVM, st v1alpha1.FirecrackerVMStatus) error {
	prev := vm.Status.Phase
	vm.Status = firecracker.Finish(st, vm.Generation)
	if err := req.ApplyStatus(ctx, vm); err != nil {
		return err
	}
	if prev != vm.Status.Phase {
		c.log.Info("phase", "firecrackervm", vm.Name, "from", string(prev), "to", string(vm.Status.Phase),
			"lostReason", string(vm.Status.LostReason), "message", vm.Status.Message)
	}
	return nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.FirecrackerVM) (ctlruntime.Result, error) {
	if !vm.DeletionTimestamp.IsZero() {
		if vm.Status.Phase != v1alpha1.ComputeVMPhaseReleasing {
			st := vm.Status
			st.Phase, st.Message = v1alpha1.ComputeVMPhaseReleasing, "graceful stop, then the work dir goes"
			if err := c.write(ctx, req, vm, st); err != nil {
				return ctlruntime.Result{}, err
			}
		}
		if err := c.p.Teardown(ctx, vm); err != nil {
			return ctlruntime.Result{}, err
		}
		c.forget(vm.Name)
		c.collectImages(ctx, req)
		c.mu.Lock()
		none := len(c.clusters) == 0
		c.mu.Unlock()
		if none {
			c.p.DropSlice(ctx)
		}
		return ctlruntime.Result{}, nil
	}
	c.remember(vm.Name, req.Cluster)
	switch vm.Status.Phase {
	case v1alpha1.ComputeVMPhaseLost, v1alpha1.ComputeVMPhaseFailed:
		return ctlruntime.Result{}, nil
	case v1alpha1.ComputeVMPhaseRunning:
		err := c.write(ctx, req, vm, c.p.Observe(ctx, vm))
		c.follow(vm)
		return ctlruntime.Result{}, err
	}
	st, err := c.p.Boot(ctx, vm, func(st v1alpha1.FirecrackerVMStatus) error { return c.write(ctx, req, vm, st) })
	if werr := c.write(ctx, req, vm, st); werr != nil && err == nil {
		err = werr
	}
	c.follow(vm)
	return ctlruntime.Result{}, err
}

func (c *Controller) follow(vm *v1alpha1.FirecrackerVM) {
	if vm.Status.Phase == v1alpha1.ComputeVMPhaseRunning {
		c.p.Follow(c.life, vm)
	}
}

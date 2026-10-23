package firecrackervm

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/compute/firecracker"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
)

var _ firecracker.Placement = (*Placement)(nil)

type Placement struct {
	hps kube.Resource

	backends kube.Resource

	log *slog.Logger
}

func NewPlacement(h *computevm.Host) *Placement {
	return &Placement{hps: h.HostProcesses, backends: h.Backends, log: h.Log().With("placement", "hostprocess")}
}

func (p *Placement) child(ctx context.Context, name string) (*v1alpha1.HostProcess, error) {
	hp, err := kube.Get[v1alpha1.HostProcess](ctx, p.hps, computevm.ChildName(name))
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return hp, err
}

func (p *Placement) Place(ctx context.Context, vm *v1alpha1.FirecrackerVM, slot int, spec host.UnitSpec) error {
	ctrl := true
	hp := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: computevm.ChildName(vm.Name),
		Labels:     map[string]string{computevm.LabelComputeVM: vm.Name, computevm.LabelSlot: strconv.Itoa(slot)},
		Finalizers: []string{v1alpha1.FinalizerFirecrackerVM},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.BackendKindFirecrackerVM,
			Name: vm.Name, UID: vm.UID, Controller: &ctrl}}},
		Spec: hostprocess.SpecOf(spec)}
	hp.Spec.Description = spec.Description
	if _, err := kube.Create[v1alpha1.HostProcess](ctx, p.hps, &hp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("firecrackervm: creating the HostProcess child %s: %w", hp.Name, err)
	}
	p.log.Info("HostProcess child created", "firecrackervm", vm.Name, "hostprocess", hp.Name, "unit", spec.Name, "slot", slot)
	return nil
}

func (p *Placement) Placed(ctx context.Context, name string) (int, bool, error) {
	hp, err := p.child(ctx, name)
	if err != nil || hp == nil {
		return 0, false, err
	}
	k, err := strconv.Atoi(hp.Labels[computevm.LabelSlot])
	if err != nil || k < 0 {
		return 0, true, nil
	}
	return k, true, nil
}

func (p *Placement) Unplace(ctx context.Context, name string) error {
	hp, err := p.child(ctx, name)
	if err != nil || hp == nil || hp.DeletionTimestamp != nil {
		return err
	}
	if err := p.hps.Delete(ctx, computevm.ChildName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	p.log.Info("HostProcess child deleted; its supervisor stops the unit", "firecrackervm", name)
	return nil
}

func (p *Placement) Settle(ctx context.Context, name string) error {
	hp, err := p.child(ctx, name)
	if err != nil || hp == nil {
		return err
	}
	return p.dropFinalizer(ctx, hp)
}

func (p *Placement) dropFinalizer(ctx context.Context, cu *v1alpha1.HostProcess) error {
	fin := cu.Finalizers
	keep := fin[:0:0]
	for _, f := range fin {
		if f != v1alpha1.FinalizerFirecrackerVM && f != v1alpha1.FinalizerComputeVM {
			keep = append(keep, f)
		}
	}
	if len(keep) == len(fin) {
		return nil
	}
	next := cu.DeepCopy()
	next.Finalizers = keep
	_, err := kube.Update[v1alpha1.HostProcess](ctx, p.hps, next, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (p *Placement) exists(ctx context.Context, name string) (bool, error) {
	_, err := kube.Get[v1alpha1.FirecrackerVM](ctx, p.backends, name)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (p *Placement) collectChildren(ctx context.Context, live func(ctx context.Context, name string) (bool, error)) {
	children, err := kube.List[v1alpha1.HostProcess](ctx, p.hps, metav1.ListOptions{LabelSelector: computevm.LabelComputeVM})
	if err != nil {
		return
	}
	for i := range children {
		c := &children[i]
		owner := c.Labels[computevm.LabelComputeVM]
		if ok, err := p.exists(ctx, owner); ok || err != nil {
			continue
		}
		if c.DeletionTimestamp == nil {
			p.hps.Delete(ctx, c.Name, metav1.DeleteOptions{})
		}
		if running, err := live(ctx, owner); err == nil && !running {
			p.dropFinalizer(ctx, c)
			p.log.Info("a HostProcess child with no FirecrackerVM is removed", "hostprocess", c.Name)
		}
	}
}

package firecrackervm

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/compute/firecracker"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

var slotRE = regexp.MustCompile(`slot=(\d+)`)

var _ firecracker.Placement = (*Placement)(nil)

type Placement struct {
	hps dynamic.ResourceInterface

	backends dynamic.ResourceInterface

	log *slog.Logger
}

func NewPlacement(h *computevm.Host) *Placement {
	return &Placement{hps: h.HostProcesses, backends: h.Backends, log: h.Log().With("placement", "hostprocess")}
}

func (p *Placement) child(ctx context.Context, name string) (*unstructured.Unstructured, *v1alpha1.HostProcess, error) {
	u, err := p.hps.Get(ctx, computevm.ChildName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	hp, err := unstruct.From[v1alpha1.HostProcess](u)
	return u, &hp, err
}

func (p *Placement) Place(ctx context.Context, vm *v1alpha1.FirecrackerVM, slot int, spec host.UnitSpec) error {
	ctrl := true
	hp := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: computevm.ChildName(vm.Name),
		Labels:      map[string]string{computevm.LabelComputeVM: vm.Name},
		Annotations: map[string]string{hostprocess.AnnotationDescription: spec.Description},
		Finalizers:  []string{v1alpha1.FinalizerFirecrackerVM},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.BackendKindFirecrackerVM,
			Name: vm.Name, UID: vm.UID, Controller: &ctrl}}},
		Spec: hostprocess.SpecOf(spec)}
	u, err := unstruct.To(&hp, "HostProcess")
	if err != nil {
		return err
	}
	if _, err := p.hps.Create(ctx, u, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("firecrackervm: creating the HostProcess child %s: %w", hp.Name, err)
	}
	p.log.Info("HostProcess child created", "firecrackervm", vm.Name, "hostprocess", hp.Name, "unit", spec.Name, "slot", slot)
	return nil
}

func (p *Placement) Placed(ctx context.Context, name string) (int, bool, error) {
	_, hp, err := p.child(ctx, name)
	if err != nil || hp == nil {
		return 0, false, err
	}
	m := slotRE.FindStringSubmatch(hp.Annotations[hostprocess.AnnotationDescription])
	if m == nil {
		return 0, true, nil
	}
	k, _ := strconv.Atoi(m[1])
	return k, true, nil
}

func (p *Placement) Unplace(ctx context.Context, name string) error {
	cu, hp, err := p.child(ctx, name)
	if err != nil || hp == nil || cu.GetDeletionTimestamp() != nil {
		return err
	}
	if err := p.hps.Delete(ctx, computevm.ChildName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	p.log.Info("HostProcess child deleted; its supervisor stops the unit", "firecrackervm", name)
	return nil
}

func (p *Placement) Settle(ctx context.Context, name string) error {
	cu, hp, err := p.child(ctx, name)
	if err != nil || hp == nil {
		return err
	}
	return p.dropFinalizer(ctx, cu)
}

func (p *Placement) dropFinalizer(ctx context.Context, cu *unstructured.Unstructured) error {
	fin := cu.GetFinalizers()
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
	next.SetFinalizers(keep)
	_, err := p.hps.Update(ctx, next, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (p *Placement) exists(ctx context.Context, name string) (bool, error) {
	_, err := p.backends.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (p *Placement) collectChildren(ctx context.Context, live func(ctx context.Context, name string) (bool, error)) {
	children, err := p.hps.List(ctx, metav1.ListOptions{LabelSelector: computevm.LabelComputeVM})
	if err != nil {
		return
	}
	for i := range children.Items {
		c := &children.Items[i]
		owner := c.GetLabels()[computevm.LabelComputeVM]
		if ok, err := p.exists(ctx, owner); ok || err != nil {
			continue
		}
		if c.GetDeletionTimestamp() == nil {
			p.hps.Delete(ctx, c.GetName(), metav1.DeleteOptions{})
		}
		if running, err := live(ctx, owner); err == nil && !running {
			p.dropFinalizer(ctx, c)
			p.log.Info("a HostProcess child with no FirecrackerVM is removed", "hostprocess", c.GetName())
		}
	}
}

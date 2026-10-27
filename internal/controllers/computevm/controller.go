package computevm

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/compute/firecracker"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	Name = "computevm"

	Export = "computevms"

	Workers = 4
)

type Controller struct {
	host *computevm.Host

	handle string

	log *slog.Logger
}

func Register(m *ctlruntime.Manager, host *computevm.Host) (*Controller, error) {
	handle := host.Config().Handle
	ctl := &Controller{host: host, handle: handle, log: host.Log().With("controller", Name)}
	if err := ctlruntime.For[*v1alpha1.ComputeVM](m, Name, ctl, ctlruntime.Export(Export),
		ctlruntime.FieldManager(v1alpha1.FieldManagerComputeVMController), ctlruntime.Finalizer(v1alpha1.FinalizerComputeVM),
		ctlruntime.SharedStatus(), ctlruntime.Workers(Workers), ctlruntime.Owns[*v1alpha1.FirecrackerVM](),
		ctlruntime.Only(func(vm *v1alpha1.ComputeVM) bool { return vm.Spec.Owner.Handle == handle })); err != nil {
		return nil, fmt.Errorf("computevm: registering the ComputeVM controller: %w", err)
	}
	return ctl, nil
}

func setCondition(st *v1alpha1.ComputeVMStatus, gen int64, t string, ok bool, reason, msg string) {
	conds.Set(&st.Conditions, gen, t, ok, reason, computevm.CapMessage(msg))
}

func (c *Controller) backend(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.ComputeVM) (*v1alpha1.FirecrackerVM, error) {
	var fvm v1alpha1.FirecrackerVM
	err := req.Get(ctx, "", vm.Spec.BackendRef.Name, &fvm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fvm, nil
}

func isSandbox(vm *v1alpha1.ComputeVM) bool {
	ref := vm.Spec.BackendRef
	return ref.Kind == v1alpha1.BackendKindDenoSandbox && (ref.Group == "" || ref.Group == v1alpha1.GroupName) && ref.Name != ""
}

func (c *Controller) reconcileSandbox(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.ComputeVM) (ctlruntime.Result, error) {
	if !vm.DeletionTimestamp.IsZero() || computevm.ReleaseRequested(vm) {
		return c.release(ctx, req, vm)
	}
	st := vm.Status
	st.Conditions = append([]metav1.Condition(nil), st.Conditions...)
	st.Phase, st.Message = v1alpha1.ComputeVMPhaseRunning, "the deno sandbox runs on the bidder"
	setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionBackendBound, true, "DenoSandbox", "the bidder runs the sandbox itself")
	setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionReady, true, "Ready", st.Message)
	return ctlruntime.Result{}, c.write(ctx, req, vm, st)
}

func backendSupported(vm *v1alpha1.ComputeVM) bool {
	ref := vm.Spec.BackendRef
	return ref.Kind == v1alpha1.BackendKindFirecrackerVM && (ref.Group == "" || ref.Group == v1alpha1.GroupName) && ref.Name != ""
}

func (c *Controller) bind(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.ComputeVM, have *v1alpha1.FirecrackerVM) (*v1alpha1.FirecrackerVM, error) {
	spec := firecracker.SpecFor(c.handle, vm.Spec)
	if have != nil && reflect.DeepEqual(have.Spec, spec) && hasOwner(have, vm) {
		return have, nil
	}
	if have != nil && !have.DeletionTimestamp.IsZero() {
		return have, nil
	}
	ctrl := true
	fvm := &v1alpha1.FirecrackerVM{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.BackendKindFirecrackerVM},
		ObjectMeta: metav1.ObjectMeta{Name: vm.Spec.BackendRef.Name, Labels: commonv1.ContractLabels(commonv1.ContractComputeVMs),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM", Name: vm.Name, UID: vm.UID,
				Controller: &ctrl, BlockOwnerDeletion: &ctrl}}},
		Spec: spec,
	}
	if err := req.Apply(ctx, fvm); err != nil {
		return nil, err
	}
	if have == nil {
		c.log.Info("the FirecrackerVM backend is created", "computevm", vm.Name, "firecrackervm", fvm.Name)
	}
	return c.backend(ctx, req, vm)
}

func hasOwner(fvm *v1alpha1.FirecrackerVM, vm *v1alpha1.ComputeVM) bool {
	for _, o := range fvm.OwnerReferences {
		if o.UID == vm.UID && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

func mirror(st v1alpha1.ComputeVMStatus, gen int64, fvm *v1alpha1.FirecrackerVM) v1alpha1.ComputeVMStatus {
	st.Conditions = append([]metav1.Condition(nil), st.Conditions...)
	st.ObservedGeneration = gen
	if fvm == nil {
		st.Phase, st.LostReason, st.Addresses, st.BootedAt = v1alpha1.ComputeVMPhasePending, "", nil, ""
		setCondition(&st, gen, v1alpha1.ComputeVMConditionReady, false, "BackendPending", "the backend is not created yet")
		return st
	}
	b := fvm.Status
	st.Phase, st.LostReason, st.BootedAt, st.Message = b.Phase, b.LostReason, b.BootedAt, b.Message
	if st.Phase == "" {
		st.Phase = v1alpha1.ComputeVMPhasePending
	}
	st.Addresses = nil
	if n := b.Network; n != nil && (n.GuestIP != "" || n.HostAddress != "") {
		st.Addresses = &v1alpha1.ComputeVMAddresses{Guest: n.GuestIP, Host: n.HostAddress}
	}
	ready := false
	reason, msg := "BackendNotReady", "the FirecrackerVM "+fvm.Name+" is not Ready"
	if c := apimeta.FindStatusCondition(b.Conditions, v1alpha1.FirecrackerVMConditionReady); c != nil && b.ObservedGeneration == fvm.Generation {
		ready = c.Status == metav1.ConditionTrue
		reason, msg = c.Reason, c.Message
	}
	setCondition(&st, gen, v1alpha1.ComputeVMConditionReady, ready, reason, msg)
	return st
}

func (c *Controller) write(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.ComputeVM, st v1alpha1.ComputeVMStatus) error {
	st.Message = computevm.CapMessage(st.Message)
	prev := vm.Status.Phase
	if reflect.DeepEqual(vm.Status, st) {
		return nil
	}
	next := vm.DeepCopy()
	next.Status = st
	next.Status.Market = nil
	if err := req.ApplyStatus(ctx, next); err != nil {
		return err
	}
	if prev != st.Phase {
		c.log.Info("phase", "computevm", vm.Name, "from", string(prev), "to", string(st.Phase), "lostReason", string(st.LostReason),
			"message", st.Message)
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (c *Controller) declare(ctx context.Context, req ctlruntime.Request, drop string) {
	var list v1alpha1.ComputeVMList
	if err := req.List(ctx, &list); err != nil {
		c.log.Warn("listing the ComputeVMs to declare their guest material", "err", err.Error())
		return
	}
	names := make([]string, 0, len(list.Items))
	for _, vm := range list.Items {
		if isSandbox(&vm) {
			continue
		}
		if vm.Spec.Owner.Handle == c.handle && vm.Name != drop {
			names = append(names, vm.Name)
		}
	}
	if err := c.host.DeclareGuests(ctx, names); err != nil {
		c.log.Warn("declaring the live guests' material in the bidder's vault", "err", err.Error())
	}
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.ComputeVM) (ctlruntime.Result, error) {
	c.declare(ctx, req, "")
	if !vm.DeletionTimestamp.IsZero() {
		return c.release(ctx, req, vm)
	}
	st := vm.Status
	st.Conditions = append([]metav1.Condition(nil), st.Conditions...)
	if isSandbox(vm) {
		return c.reconcileSandbox(ctx, req, vm)
	}
	if !backendSupported(vm) {
		setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionBackendBound, false, "UnsupportedBackend",
			fmt.Sprintf("spec.backendRef names %s/%s; this host runs FirecrackerVM", vm.Spec.BackendRef.Kind, vm.Spec.BackendRef.Name))
		setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionReady, false, "UnsupportedBackend", "no backend is bound")
		return ctlruntime.Result{}, c.write(ctx, req, vm, st)
	}
	if computevm.ReleaseRequested(vm) {
		observedAt := computevm.ReleaseRequestedAt(vm)
		if observedAt == "" {
			observedAt = now()
		}
		if err := c.host.EnsureTerminationSync(ctx, *vm, "deleted_on_request", observedAt, computevm.ReleaseRequestRef(vm)); err != nil {
			return ctlruntime.Result{}, err
		}
		c.log.Info("the creator asks for the release; the ComputeVM is deleted and its finalizer releases the backend", "computevm", vm.Name)
		return ctlruntime.Result{}, req.Delete(ctx, vm)
	}
	have, err := c.backend(ctx, req, vm)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if have == nil {
		if err := c.host.EnsureGuestLogin(ctx, *vm); err != nil {
			if computevm.RefusedLogin(err) {
				st.Phase, st.Message = v1alpha1.ComputeVMPhaseFailed, err.Error()
				setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionReady, false, "GuestLoginRefused", err.Error())
				return ctlruntime.Result{}, c.write(ctx, req, vm, st)
			}
			st.Phase, st.Message = v1alpha1.ComputeVMPhasePending, "waiting for the guest login role: "+err.Error()
			if werr := c.write(ctx, req, vm, st); werr != nil {
				return ctlruntime.Result{}, werr
			}
			return ctlruntime.Result{}, err
		}
	}
	fvm, err := c.bind(ctx, req, vm, have)
	if err != nil {
		setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionBackendBound, false, "BindFailed", err.Error())
		if werr := c.write(ctx, req, vm, st); werr != nil {
			return ctlruntime.Result{}, werr
		}
		return ctlruntime.Result{}, err
	}
	st = mirror(st, vm.Generation, fvm)
	if fvm != nil {
		setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionBackendBound, true, "Bound", "FirecrackerVM "+fvm.Name)
	} else {
		setCondition(&st, vm.Generation, v1alpha1.ComputeVMConditionBackendBound, false, "BackendMissing", "FirecrackerVM "+vm.Spec.BackendRef.Name+" does not exist")
	}
	if computevm.Ended(&v1alpha1.ComputeVM{Status: st}) {
		if err := c.host.EnsureTerminationSync(ctx, *vm, computevm.GoneReason(st.LostReason), now(), nil); err != nil {
			c.log.Warn("creating the termination StateSync of an ended ComputeVM failed", "computevm", vm.Name, "err", err.Error())
		}
	}
	return ctlruntime.Result{}, c.write(ctx, req, vm, st)
}

func (c *Controller) release(ctx context.Context, req ctlruntime.Request, vm *v1alpha1.ComputeVM) (ctlruntime.Result, error) {
	observedAt := computevm.ReleaseRequestedAt(vm)
	if observedAt == "" {
		observedAt = vm.DeletionTimestamp.UTC().Format(time.RFC3339)
	}
	if err := c.host.EnsureTerminationSync(ctx, *vm, "deleted_on_request", observedAt, computevm.ReleaseRequestRef(vm)); err != nil {
		return ctlruntime.Result{}, err
	}
	fvm, err := c.backend(ctx, req, vm)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	st := mirror(vm.Status, vm.Generation, fvm)
	if fvm != nil {
		if st.Phase != v1alpha1.ComputeVMPhaseReleasing {
			st.Phase, st.Message = v1alpha1.ComputeVMPhaseReleasing, "the backend "+fvm.Name+" is released"
		}
		if err := c.write(ctx, req, vm, st); err != nil {
			return ctlruntime.Result{}, err
		}
		if fvm.DeletionTimestamp.IsZero() {
			if err := req.Delete(ctx, fvm); err != nil {
				return ctlruntime.Result{}, err
			}
			c.log.Info("the FirecrackerVM backend is deleted; its controller stops the VM", "computevm", vm.Name, "firecrackervm", fvm.Name)
		}
		return ctlruntime.Result{Waiting: true}, nil
	}
	c.host.DropGuestLogin(ctx, *vm)
	c.host.DropGuestGrants(ctx, vm.Name)
	c.declare(ctx, req, vm.Name)
	c.log.Info("released: the backend is gone and the finalizer is dropped", "computevm", vm.Name)
	return ctlruntime.Result{}, nil
}

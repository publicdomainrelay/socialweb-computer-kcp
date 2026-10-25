package firecracker

import (
	"context"
	"errors"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/compute"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

var _ compute.Provider = (*Provider)(nil)

func SpecFor(handle string, vm v1alpha1.ComputeVMSpec) v1alpha1.FirecrackerVMSpec {
	return v1alpha1.FirecrackerVMSpec{Owner: v1alpha1.ComputeVMOwner{Handle: handle}, Vcpu: vm.VM.Cpus, Mem: vm.VM.Mem, Disk: vm.VM.Disk,
		Image: vm.Image, UserData: vm.UserData, StopGraceSeconds: vm.StopGraceSeconds, HostPorts: slices.Clone(vm.HostPorts),
		GuestNetwork: v1alpha1.GuestNetworkWith(vm.GuestNetwork.LANUDPAllowed(), vm.GuestNetwork.HostUDPAllowed(), vm.GuestNetwork.WebRTCUDPPortOf())}
}

func (p *Provider) ref(name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: v1alpha1.BackendKindFirecrackerVM, Name: name}
}

func instanceOf(ref commonv1.ObjectRef, st v1alpha1.FirecrackerVMStatus) compute.Instance {
	in := compute.Instance{Ref: ref, Phase: string(st.Phase), Message: st.Message}
	if n := st.Network; n != nil {
		for _, a := range []string{n.GuestIP, n.HostAddress} {
			if a != "" {
				in.Addresses = append(in.Addresses, a)
			}
		}
	}
	return in
}

func (p *Provider) Provision(ctx context.Context, req compute.ProvisionRequest) (compute.Instance, error) {
	if req.Name == "" {
		return compute.Instance{}, errors.New("firecracker: the provision request names no VM")
	}
	vm := &v1alpha1.FirecrackerVM{ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec: v1alpha1.FirecrackerVMSpec{Owner: v1alpha1.ComputeVMOwner{Handle: p.handle}, Vcpu: req.Shape.CPUs, Mem: req.Shape.Mem,
			Disk: req.Shape.Disk, UserData: v1alpha1.ComputeVMUserData{Vault: req.UserData}}}
	st, err := p.Boot(ctx, vm, func(v1alpha1.FirecrackerVMStatus) error { return nil })
	return instanceOf(p.ref(req.Name), st), err
}

func (p *Provider) Instance(ctx context.Context, ref commonv1.ObjectRef) (compute.Instance, error) {
	rec, err := p.rt.Load(p.handle, ref.Name)
	if err != nil {
		return compute.Instance{Ref: ref, Phase: string(v1alpha1.ComputeVMPhasePending)}, nil
	}
	vm := &v1alpha1.FirecrackerVM{ObjectMeta: metav1.ObjectMeta{Name: ref.Name}, Spec: v1alpha1.FirecrackerVMSpec{Owner: v1alpha1.ComputeVMOwner{Handle: p.handle}},
		Status: runningStatus(v1alpha1.FirecrackerVMStatus{}, 0, rec)}
	return instanceOf(ref, p.Observe(ctx, vm)), nil
}

func (p *Provider) Release(ctx context.Context, ref commonv1.ObjectRef) error {
	vm := &v1alpha1.FirecrackerVM{ObjectMeta: metav1.ObjectMeta{Name: ref.Name}, Spec: v1alpha1.FirecrackerVMSpec{Owner: v1alpha1.ComputeVMOwner{Handle: p.handle}}}
	if rec, err := p.rt.Load(p.handle, ref.Name); err == nil {
		vm.Status = runningStatus(vm.Status, 0, rec)
	}
	return p.Teardown(ctx, vm)
}

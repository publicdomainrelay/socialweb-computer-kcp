package bidder

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

func (b *Bidder) provisionSandbox(ctx context.Context, req provisionRequest, record vmRecord) (string, error) {
	name, err := vmNameForAccept(req.requesterDID, b.guests.AcceptKey(req.acceptURI, req.acceptCID))
	if err != nil {
		return "", err
	}
	vm := &v1alpha1.ComputeVM{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ComputeVMSpec{
			BackendRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: v1alpha1.BackendKindDenoSandbox, Name: name},
			VM:         v1alpha1.ComputeVMShape{Cpus: record.CPUs, Mem: record.Mem, Disk: record.Disk, Role: record.Role},
			Owner:      v1alpha1.ComputeVMOwner{Handle: b.cfg.ProviderHandle},
			Requester:  v1alpha1.ComputeVMRequester{Did: req.requesterDID},
			Accept:     &commonv1.StrongRef{URI: req.acceptURI, CID: req.acceptCID},
			Sandbox:    &v1alpha1.ComputeVMSandbox{Source: record.UserData},
		},
	}
	if req.owner != nil {
		vm.OwnerReferences = []metav1.OwnerReference{*req.owner}
	}
	if err := b.intents.CreateComputeVM(ctx, *vm); err != nil && !errors.Is(err, contract.ErrExists) {
		return "", fmt.Errorf("bidder: creating the deno sandbox ComputeVM: %w", err)
	}
	b.log.Info("a deno sandbox ComputeVM is applied", "computevm", name, "role", record.Role, "requester", req.requesterDID)
	return name, nil
}

func (b *Bidder) observeSandbox(vm *v1alpha1.ComputeVM) {
	if vm.Spec.BackendRef.Kind != v1alpha1.BackendKindDenoSandbox {
		return
	}
	if b.guests.Releasing(vm) || b.guests.ReleaseRequested(vm) || !vm.DeletionTimestamp.IsZero() {
		b.stopSandbox(vm.Name)
		return
	}
	if vm.Spec.Contract == nil || vm.Spec.Contract.Receipt.URI == "" {
		return
	}
	if err := b.startSandbox(context.Background(), vm); err != nil {
		b.log.Warn("starting a deno sandbox failed", "computevm", vm.Name, "err", err.Error())
	}
}

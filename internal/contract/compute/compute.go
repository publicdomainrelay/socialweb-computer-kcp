package compute

import (
	"context"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	CapabilityProvision = "compute.provision"

	CapabilityRelease = "compute.release"
)

type Shape struct {
	CPUs int32

	Mem string

	Disk string
}

type ProvisionRequest struct {
	Name string

	Owner commonv1.ObjectRef

	Requester string

	Role string

	Shape Shape

	UserData commonv1.SecretRef

	Contract *commonv1.StrongRef
}

type Instance struct {
	Ref commonv1.ObjectRef

	Phase string

	Addresses []string

	Message string
}

type Provider interface {
	Provision(ctx context.Context, req ProvisionRequest) (Instance, error)

	Instance(ctx context.Context, ref commonv1.ObjectRef) (Instance, error)

	Release(ctx context.Context, ref commonv1.ObjectRef) error
}

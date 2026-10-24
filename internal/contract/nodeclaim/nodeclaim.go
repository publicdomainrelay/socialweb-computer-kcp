package nodeclaim

import (
	"context"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	Kind = "NodeClaim"

	FieldManager = "socialweb.computer/nodeclaim"

	AgentAccount = "socialweb-computer-k8s"

	AgentNamespace = "default"

	AgentWorkspace = "app"
)

func AgentRef(workspace string) string {
	return workspace + ":" + AgentWorkspace + "/" + AgentNamespace + "/" + AgentAccount
}

type Claim struct {
	Workspace string

	Object v1alpha1.NodeClaim
}

func (c Claim) Key() string { return c.Workspace + "/" + c.Object.Name }

func (c Claim) Ref() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: c.Object.Name, Workspace: c.Workspace}
}

type Link struct {
	Name string

	Status *v1alpha1.ClusterLinkStatus

	Err error
}

type Links struct {
	OpenBao Link

	Apiserver Link
}

type Bases struct {
	Meshd string

	NodeAgent string
}

type Settlement struct {
	Status *v1alpha1.NodeClaimStatus

	RemoveBundle bool

	Done bool
}

type Mapper interface {
	InfraName(claim Claim) string

	EntryName(claim Claim) string

	BundleRef(contract string) commonv1.SecretRef

	Wants(claim Claim) (ensure, adopt bool)

	State(claim Claim) v1alpha1.InfraContractState

	Bases(ctx context.Context, adopt bool) (Bases, bool)

	LinkNames(claim Claim) (openBao, apiserver string)

	Bundle(claim Claim, links Links) (map[string]any, error)

	Entry(claim Claim, adopt bool, bases Bases) (v1alpha1.InfraContract, v1alpha1.InfraContractNode)

	Requested(status v1alpha1.NodeClaimStatus, adopt bool) v1alpha1.NodeClaimStatus

	Failed(status v1alpha1.NodeClaimStatus, cause error) v1alpha1.NodeClaimStatus

	Status(claim Claim, contract v1alpha1.ComputeContract) *v1alpha1.NodeClaimStatus

	Settle(claim Claim, contract *v1alpha1.ComputeContract) Settlement
}

type Binding struct {
	Owner commonv1.ObjectRef

	Mapper Mapper

	Vault commonv1.ObjectRef

	ContractWorkspace string

	IndexWorkspace string

	IndexNamespace string

	Homes []string

	Workspace string
}

type Binder interface {
	Bind(b Binding)

	Unbind(owner commonv1.ObjectRef)
}

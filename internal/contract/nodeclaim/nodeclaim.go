package nodeclaim

import (
	"context"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	Kind = "NodeClaim"

	FieldManager = "socialweb.computer/nodeclaim"

	UIDLabel = "socialweb.computer/nodeclaim-uid"

	ContractTTLSeconds int32 = 691200

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
	Release bool

	Status *v1alpha1.NodeClaimStatus

	DeleteContract bool

	RemoveBundle bool

	Done bool
}

type Orphaned struct {
	Release bool

	Delete bool
}

type Mapper interface {
	ContractName(claim Claim) string

	BundleRef(contract string) commonv1.SecretRef

	Wants(claim Claim) (ensure, adopt bool)

	Bases(ctx context.Context, adopt bool) (Bases, bool)

	LinkNames(claim Claim) (openBao, apiserver string)

	Bundle(claim Claim, links Links) (map[string]any, error)

	Contract(claim Claim, name string, adopt bool, bases Bases) v1alpha1.ComputeContractSpec

	Requested(status v1alpha1.NodeClaimStatus, adopt bool) v1alpha1.NodeClaimStatus

	Failed(status v1alpha1.NodeClaimStatus, cause error) v1alpha1.NodeClaimStatus

	Status(claim Claim, contract v1alpha1.ComputeContract) *v1alpha1.NodeClaimStatus

	Orphan(claim *Claim, contract v1alpha1.ComputeContract) (Orphaned, bool)

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

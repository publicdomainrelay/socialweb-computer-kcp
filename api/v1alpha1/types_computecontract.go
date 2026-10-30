package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ComputeContract struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComputeContractSpec   `json:"spec,omitempty"`
	Status ComputeContractStatus `json:"status,omitempty"`
}

type ComputeContractList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ComputeContract `json:"items"`
}

type ComputeContractTemplate struct {
	VMName string `json:"vmName,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	DenyBidderDids []string `json:"denyBidderDids,omitempty"`

	SshAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`

	UserData string `json:"userData,omitempty"`

	ComputeKind ComputeKind `json:"computeKind,omitempty" apigen:"default=vm,enum=vm|deno"`

	Policy string `json:"policy,omitempty"`

	PolicyArgs string `json:"policyArgs,omitempty"`

	VMReadyTimeoutSec int `json:"vmReadyTimeoutSec,omitempty"`

	Tunnels []ComputeContractTunnel `json:"tunnels,omitempty" apigen:"listType=map,listMapKey=tag"`

	Agent *ComputeContractAgent `json:"agent,omitempty"`
}

type ComputeContractSpec struct {
	ComputeContractTemplate `json:",inline"`

	SecretsRef *commonv1.SecretRef `json:"secretsRef,omitempty"`

	Release bool `json:"release,omitempty"`

	OwnerDid string `json:"ownerDid,omitempty"`

	Adopt bool `json:"adopt,omitempty"`

	NodeJoin *ComputeContractNodeJoin `json:"nodeJoin,omitempty"`

	Association *ComputeContractAssociation `json:"association,omitempty"`

	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty" apigen:"default=691200,minimum=0"`
}

type ContractBidVerdict string

const (
	ContractBidConsidered ContractBidVerdict = "considered"

	ContractBidDenied ContractBidVerdict = "denied"

	ContractBidChosen ContractBidVerdict = "chosen"

	ContractBidNotChosen ContractBidVerdict = "notChosen"

	ContractBidPolicyRejected ContractBidVerdict = "policyRejected"
)

type ContractBid struct {
	Did string `json:"did"`

	URI string `json:"uri"`

	Offer string `json:"offer,omitempty"`

	Cost int64 `json:"cost,omitempty"`

	Verdict ContractBidVerdict `json:"verdict"`

	ArrivedAt string `json:"arrivedAt"`
}

type ContractChosenReason string

const (
	ContractChosenFirstFree ContractChosenReason = "firstFree"

	ContractChosenBestWithinWindow ContractChosenReason = "bestWithinWindow"

	ContractChosenPolicy ContractChosenReason = "policy"

	ContractChosenResumed ContractChosenReason = "resumed"
)

type ContractChosen struct {
	URI string `json:"uri"`

	Did string `json:"did"`

	Reason ContractChosenReason `json:"reason"`

	Considered int `json:"considered"`
}

type ComputeContractNodeJoin struct {
	ClusterRef commonv1.LocalObjectRef `json:"clusterRef"`

	Meshd *ComputeContractMeshd `json:"meshd,omitempty"`

	NodeAgent *ComputeContractNodeAgent `json:"nodeAgent,omitempty"`
}

type ComputeContractTunnel struct {
	Tag string `json:"tag" apigen:"pattern=^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$"`

	Port int `json:"port" apigen:"minimum=1,maximum=65535"`

	Server string `json:"server,omitempty" apigen:"userEndpoint"`

	Shell *ComputeContractShell `json:"shell,omitempty"`

	Transport TunnelTransport `json:"transport,omitempty" apigen:"default=opentunnel,enum=opentunnel|webrtc"`
}

type ComputeContractShell struct {
	EnvFile string `json:"envFile,omitempty" apigen:"userEndpoint,default=/etc/wootty/wootty.env"`
}

type ComputeContractAgent struct {
	EnvFile string `json:"envFile,omitempty" apigen:"userEndpoint,default=/etc/atirc/atirc.env"`
}

type ComputeContractAssociation struct {
	Subject string `json:"subject"`

	RefFile string `json:"refFile,omitempty" apigen:"userEndpoint,default=/etc/socialweb-computer/association-ref"`
}

type ComputeKind string

const (
	ComputeKindVM ComputeKind = "vm"

	ComputeKindDeno ComputeKind = "deno"
)

type TunnelTransport string

const (
	TunnelTransportOpenTunnel TunnelTransport = "opentunnel"

	TunnelTransportWebRTC TunnelTransport = "webrtc"
)

type ComputeContractNodeAgent struct {
	DownloadBase string `json:"downloadBase"`

	Pins []ComputeContractMeshdPin `json:"pins"`
}

type ComputeContractMeshd struct {
	DownloadBase string `json:"downloadBase"`

	Pins []ComputeContractMeshdPin `json:"pins"`
}

type ComputeContractMeshdPin struct {
	Arch string `json:"arch"`

	Sha256 string `json:"sha256"`
}

type ComputeContractPhase string

const (
	ComputeContractPending ComputeContractPhase = "Pending"

	ComputeContractRequesting ComputeContractPhase = "Requesting"

	ComputeContractHeld ComputeContractPhase = "Held"

	ComputeContractReleasing ComputeContractPhase = "Releasing"

	ComputeContractReleased ComputeContractPhase = "Released"

	ComputeContractFailed ComputeContractPhase = "Failed"
)

const FinalizerComputeContract = "computecontract.socialweb.computer/release"

func (c *ComputeContract) HoldsGuest() bool {
	state := c.Status.Contract
	if state == nil || state.Termination != nil {
		return false
	}
	if c.Status.Phase == ComputeContractReleased || c.Status.Phase == ComputeContractFailed {
		return false
	}
	return state.Receipt != nil || state.AcceptURI != "" || (state.Accept != nil && state.Accept.URI != "")
}

type ComputeContractRelease struct {
	StartedAt string `json:"startedAt,omitempty"`

	Deadline string `json:"deadline,omitempty"`

	Attempts int `json:"attempts,omitempty"`

	LastError string `json:"lastError,omitempty"`
}

type ComputeContractEvent struct {
	URI string `json:"uri"`

	CID string `json:"cid,omitempty"`

	ReceiptURI string `json:"receiptUri,omitempty"`

	ReceivedAt string `json:"receivedAt,omitempty"`
}

type ComputeContractStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ComputeContractPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	SecretsURL string `json:"secretsUrl,omitempty"`

	SecretsFetchedAt string `json:"secretsFetchedAt,omitempty"`

	Contract *NodeClaimContract `json:"contract,omitempty"`

	Grant *NodeClaimGrant `json:"grant,omitempty"`

	Bids []ContractBid `json:"bids,omitempty"`

	Chosen *ContractChosen `json:"chosen,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Execution *ExecutionStatus `json:"execution,omitempty"`

	Release *ComputeContractRelease `json:"release,omitempty"`

	PendingEvents []ComputeContractEvent `json:"pendingEvents,omitempty"`

	Association *commonv1.StrongRef `json:"association,omitempty"`
}

const (
	ComputeContractConditionResumed = "Resumed"

	ComputeContractConditionProgressing = "Progressing"

	ComputeContractConditionHeld = "Held"

	ComputeContractConditionReleaseAcknowledged = "ReleaseAcknowledged"

	ComputeContractConditionReleaseFailed = "ReleaseFailed"

	ComputeContractConditionTerminated = "Terminated"
)

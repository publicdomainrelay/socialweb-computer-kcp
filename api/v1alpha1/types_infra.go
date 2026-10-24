package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type Infra struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InfraSpec   `json:"spec,omitempty"`
	Status InfraStatus `json:"status,omitempty"`
}

type InfraList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Infra `json:"items"`
}

type EphemeralInfra struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EphemeralInfraSpec   `json:"spec,omitempty"`
	Status EphemeralInfraStatus `json:"status,omitempty"`
}

type EphemeralInfraList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []EphemeralInfra `json:"items"`
}

type InfraSpec struct {
	Description string `json:"description,omitempty" apigen:"maxLength=1024"`

	Contracts []InfraContract `json:"contracts,omitempty"`

	Secrets []InfraContractSecret `json:"secrets,omitempty" apigen:"listType=map,listMapKey=contract"`

	Nodes []InfraContractNode `json:"nodes,omitempty" apigen:"listType=map,listMapKey=contract"`

	ClusterRef *commonv1.ObjectRef `json:"clusterRef,omitempty"`

	Record *InfraRecordBinding `json:"record,omitempty"`
}

type InfraContractState string

const (
	InfraContractStaged InfraContractState = "Staged"

	InfraContractRequested InfraContractState = "Requested"

	InfraContractReleased InfraContractState = "Released"
)

type InfraContract struct {
	Name string `json:"name" apigen:"maxLength=63,pattern=^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$"`

	State InfraContractState `json:"state,omitempty" apigen:"default=Staged,enum=Staged|Requested|Released"`

	VMName string `json:"vmName,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	DenyBidderDids []string `json:"denyBidderDids,omitempty"`

	SshAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`

	UserData string `json:"userData,omitempty"`

	Policy string `json:"policy,omitempty"`

	PolicyArgs string `json:"policyArgs,omitempty"`

	VMReadyTimeoutSec int `json:"vmReadyTimeoutSec,omitempty"`
}

type InfraContractSecret struct {
	Contract string `json:"contract"`

	SecretsRef commonv1.SecretRef `json:"secretsRef"`
}

type InfraContractNode struct {
	Contract string `json:"contract"`

	OwnerDid string `json:"ownerDid,omitempty"`

	Adopt bool `json:"adopt,omitempty"`

	NodeJoin *ComputeContractNodeJoin `json:"nodeJoin,omitempty"`

	NodeClaimRef commonv1.ObjectRef `json:"nodeClaimRef"`
}

type InfraRecordBinding struct {
	RepoRef commonv1.ObjectRef `json:"repoRef"`

	URI string `json:"uri,omitempty" apigen:"userEndpoint"`

	EventSourceRefs []commonv1.ObjectRef `json:"eventSourceRefs,omitempty"`
}

type EphemeralInfraEnd string

const (
	EphemeralInfraEndComplete EphemeralInfraEnd = "Complete"

	EphemeralInfraEndDeadline EphemeralInfraEnd = "Deadline"
)

type EphemeralInfraSpec struct {
	InfraSpec `json:",inline"`

	TTLSeconds *int32 `json:"ttlSeconds,omitempty" apigen:"default=3600,minimum=60"`

	EndWhen EphemeralInfraEnd `json:"endWhen,omitempty" apigen:"default=Complete,enum=Complete|Deadline"`
}

type InfraPhase string

const (
	InfraPhasePending InfraPhase = "Pending"

	InfraPhaseStaged InfraPhase = "Staged"

	InfraPhaseDeploying InfraPhase = "Deploying"

	InfraPhaseDeployed InfraPhase = "Deployed"

	InfraPhaseReleasing InfraPhase = "Releasing"

	InfraPhaseReleased InfraPhase = "Released"

	InfraPhaseDegraded InfraPhase = "Degraded"

	InfraPhaseEnding InfraPhase = "Ending"

	InfraPhaseDeleting InfraPhase = "Deleting"
)

type InfraContractPhase string

const (
	InfraContractPhaseStaged InfraContractPhase = "Staged"

	InfraContractPhaseRequesting InfraContractPhase = "Requesting"

	InfraContractPhaseDeployed InfraContractPhase = "Deployed"

	InfraContractPhaseReleasing InfraContractPhase = "Releasing"

	InfraContractPhaseReleased InfraContractPhase = "Released"

	InfraContractPhaseFailed InfraContractPhase = "Failed"
)

type InfraContractStatus struct {
	Name string `json:"name"`

	Phase InfraContractPhase `json:"phase"`

	ComputeContract string `json:"computeContract,omitempty"`

	Winner string `json:"winner,omitempty"`

	ReceiptURI string `json:"receiptUri,omitempty"`

	Message string `json:"message,omitempty"`
}

type InfraSyncDirection string

const (
	InfraSyncCreated InfraSyncDirection = "Created"

	InfraSyncPushed InfraSyncDirection = "Pushed"

	InfraSyncPulled InfraSyncDirection = "Pulled"

	InfraSyncInSync InfraSyncDirection = "InSync"
)

type InfraRecordStatus struct {
	URI string `json:"uri,omitempty"`

	CID string `json:"cid,omitempty"`

	DeclarationHash string `json:"declarationHash,omitempty"`

	SyncedGeneration int64 `json:"syncedGeneration,omitempty"`

	SyncedAt string `json:"syncedAt,omitempty"`

	Direction InfraSyncDirection `json:"direction,omitempty"`
}

type InfraStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase InfraPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Contracts []InfraContractStatus `json:"contracts,omitempty"`

	Record *InfraRecordStatus `json:"record,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type EphemeralInfraStatus struct {
	InfraStatus `json:",inline"`

	ExpiresAt string `json:"expiresAt,omitempty"`

	EndedAt string `json:"endedAt,omitempty"`

	EndReason EphemeralInfraEnd `json:"endReason,omitempty"`
}

const (
	FinalizerInfra = "infra.socialweb.computer/release"

	InfraKindLabel = "infra.socialweb.computer/kind"

	InfraNameLabel = "infra.socialweb.computer/name"

	InfraContractLabel = "infra.socialweb.computer/contract"

	InfraCollectionLabel = "infra.socialweb.computer/collection"

	InfraClusterLabel = "infra.socialweb.computer/cluster"

	InfraConditionSynced = "Synced"

	InfraConditionSyncConflict = "SyncConflict"

	InfraConditionRecordValid = "RecordValid"
)

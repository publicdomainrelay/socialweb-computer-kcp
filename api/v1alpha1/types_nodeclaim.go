package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type NodeClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeClaimSpec   `json:"spec,omitempty"`
	Status NodeClaimStatus `json:"status,omitempty"`
}

type NodeClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeClaim `json:"items"`
}

type NodeClaimSpec struct {
	ClusterRef commonv1.LocalObjectRef `json:"clusterRef"`

	NodeName string `json:"nodeName"`

	OwnerDid string `json:"ownerDid"`

	ApiserverPort int `json:"apiserverPort"`

	AdvertiseAddress string `json:"advertiseAddress"`

	TokenFile string `json:"tokenFile" apigen:"userEndpoint"`

	NodePasswordFile string `json:"nodePasswordFile" apigen:"userEndpoint"`

	ServerCAFile string `json:"serverCAFile" apigen:"userEndpoint"`

	DenyBidderDids []string `json:"denyBidderDids,omitempty"`

	AdoptJoinedNode bool `json:"adoptJoinedNode,omitempty"`
}

type NodeClaimPhase string

const (
	NodeClaimPending NodeClaimPhase = "Pending"

	NodeClaimBidding NodeClaimPhase = "Bidding"

	NodeClaimProvisioning NodeClaimPhase = "Provisioning"

	NodeClaimJoining NodeClaimPhase = "Joining"

	NodeClaimRunning NodeClaimPhase = "Running"

	NodeClaimFailed NodeClaimPhase = "Failed"

	NodeClaimReleased NodeClaimPhase = "Released"

	NodeClaimLost NodeClaimPhase = "Lost"

	NodeClaimAdopting NodeClaimPhase = "Adopting"
)

type NodeClaimContract struct {
	VMURI string `json:"vmUri,omitempty"`

	RFPURI string `json:"rfpUri,omitempty"`

	AcceptURI string `json:"acceptUri,omitempty"`

	ReceiptURI string `json:"receiptUri,omitempty"`

	ReceiptCid string `json:"receiptCid,omitempty"`

	SubmitEventRef string `json:"submitEventRef,omitempty"`

	Version *int `json:"version,omitempty"`

	Phase ContractPhase `json:"phase,omitempty"`

	VMName string `json:"vmName,omitempty"`

	RequesterDid string `json:"requesterDid,omitempty"`

	MarketDid string `json:"marketDid,omitempty"`

	Payload *commonv1.StrongRef `json:"payload,omitempty"`

	RFP *commonv1.StrongRef `json:"rfp,omitempty"`

	Policy *commonv1.StrongRef `json:"policy,omitempty"`

	Bid *commonv1.StrongRef `json:"bid,omitempty"`

	Winner *ContractWinner `json:"winner,omitempty"`

	AcceptPending *ContractAcceptPending `json:"acceptPending,omitempty"`

	Accept *commonv1.StrongRef `json:"accept,omitempty"`

	Receipt *commonv1.StrongRef `json:"receipt,omitempty"`

	ReceiptOk *bool `json:"receiptOk,omitempty"`

	VMAddress string `json:"vmAddress,omitempty"`

	Bids *int `json:"bids,omitempty"`

	Release *ContractRelease `json:"release,omitempty"`

	Termination *ContractTermination `json:"termination,omitempty"`

	HostKeys *ContractHostKeys `json:"hostKeys,omitempty"`

	Outcome ContractOutcome `json:"outcome,omitempty"`

	Error string `json:"error,omitempty"`

	Timestamps *ContractTimestamps `json:"timestamps,omitempty"`
}

type ContractPhase string

const (
	ContractRequested ContractPhase = "requested"

	ContractRFPSubmitted ContractPhase = "rfp_submitted"

	ContractWinnerSelected ContractPhase = "winner_selected"

	ContractAccepted ContractPhase = "accepted"

	ContractReceipt ContractPhase = "receipt"

	ContractNetwork ContractPhase = "network"

	ContractReleased ContractPhase = "released"
)

type ContractOutcome string

const (
	ContractOutcomeNoBids ContractOutcome = "no_bids"

	ContractOutcomeBidsDenied ContractOutcome = "bids_denied"

	ContractOutcomePolicyRejected ContractOutcome = "policy_rejected"

	ContractOutcomeReturned ContractOutcome = "returned"

	ContractOutcomeReceiptInvalid ContractOutcome = "receipt_invalid"

	ContractOutcomeNetworkTimeout ContractOutcome = "network_timeout"

	ContractOutcomeComplete ContractOutcome = "complete"
)

type ContractWinner struct {
	DID string `json:"did"`

	SubmitAccept string `json:"submitAccept,omitempty"`
}

type ContractAcceptPending struct {
	BidURI string `json:"bidUri"`
}

type ContractTermination struct {
	Reason string `json:"reason"`

	ExitCode *int `json:"exitCode,omitempty"`

	ObservedAt string `json:"observedAt"`

	Event commonv1.StrongRef `json:"event"`

	Payload commonv1.StrongRef `json:"payload"`

	Request *commonv1.StrongRef `json:"request,omitempty"`
}

type ContractHostKey struct {
	Type string `json:"type"`

	PublicKey string `json:"publicKey"`

	Fingerprint string `json:"fingerprint"`
}

type ContractHostKeys struct {
	Keys []ContractHostKey `json:"keys"`

	ObservedAt string `json:"observedAt"`

	Event commonv1.StrongRef `json:"event"`

	Payload commonv1.StrongRef `json:"payload"`
}

type ContractRelease struct {
	Event commonv1.StrongRef `json:"event"`

	Payload commonv1.StrongRef `json:"payload"`

	OK bool `json:"ok"`

	Status *int `json:"status,omitempty"`
}

type ContractTimestamps struct {
	Requested string `json:"requested,omitempty"`

	RFPSubmitted string `json:"rfp_submitted,omitempty"`

	WinnerSelected string `json:"winner_selected,omitempty"`

	Accepted string `json:"accepted,omitempty"`

	Receipt string `json:"receipt,omitempty"`

	Network string `json:"network,omitempty"`

	Released string `json:"released,omitempty"`
}

type NodeClaimStatus struct {
	Phase NodeClaimPhase `json:"phase,omitempty"`

	Contract NodeClaimContract `json:"contract,omitempty"`

	Fqdn string `json:"fqdn,omitempty"`

	Message string `json:"message,omitempty"`

	Grant *NodeClaimGrant `json:"grant,omitempty"`

	Secrets *NodeClaimSecrets `json:"secrets,omitempty"`
}

type NodeClaimSecrets struct {
	IngressDid string `json:"ingressDid"`

	KeyRef string `json:"keyRef"`

	URL string `json:"url"`

	ServedAt string `json:"servedAt,omitempty"`
}

type NodeClaimGrant struct {
	Issuer string `json:"issuer"`

	Subject string `json:"subject"`

	Audience string `json:"audience"`

	Role string `json:"role,omitempty"`
}

const FinalizerNodeClaim = "nodeclaim.socialweb.computer/release"

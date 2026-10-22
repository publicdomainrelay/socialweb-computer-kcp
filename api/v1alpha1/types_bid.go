package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Bid struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BidSpec   `json:"spec,omitempty"`
	Status BidStatus `json:"status,omitempty"`
}

type BidList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Bid `json:"items"`
}

type BidRecordRef struct {
	Uri string `json:"uri"`

	Cid string `json:"cid,omitempty"`
}

type BidOffer struct {
	Kind string `json:"kind"`

	Cost int64 `json:"cost,omitempty"`
}

type BidPolicy struct {
	Scope string `json:"scope,omitempty"`

	Attached []BidRecordRef `json:"attached,omitempty"`

	Verdict string `json:"verdict"`
}

type BidSpec struct {
	Rfp BidRecordRef `json:"rfp"`

	Requester string `json:"requester"`

	PayloadNsid string `json:"payloadNsid,omitempty"`

	Offer BidOffer `json:"offer"`

	Provider string `json:"provider"`

	Policy BidPolicy `json:"policy"`

	ExpiresAt string `json:"expiresAt"`
}

type BidPhase string

const (
	BidPhaseDeciding BidPhase = "Deciding"

	BidPhasePublished BidPhase = "Published"

	BidPhaseWon BidPhase = "Won"

	BidPhaseLost BidPhase = "Lost"

	BidPhaseExpired BidPhase = "Expired"

	BidPhaseFailed BidPhase = "Failed"
)

type BidTimings struct {
	RfpSeenAt string `json:"rfpSeenAt,omitempty"`

	DecidedAt string `json:"decidedAt,omitempty"`

	PublishedAt string `json:"publishedAt,omitempty"`

	AcceptedAt string `json:"acceptedAt,omitempty"`

	FinishedAt string `json:"finishedAt,omitempty"`
}

type BidTerminated struct {
	ReceiptUri string `json:"receiptUri"`

	ReceiptCid string `json:"receiptCid,omitempty"`

	AcceptAuthor string `json:"acceptAuthor"`

	Reason string `json:"reason"`

	At string `json:"at"`

	PayloadUri string `json:"payloadUri,omitempty"`

	PayloadCid string `json:"payloadCid,omitempty"`

	EventUri string `json:"eventUri,omitempty"`

	EventCid string `json:"eventCid,omitempty"`
}

type BidStatus struct {
	Phase BidPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Bid *BidRecordRef `json:"bid,omitempty"`

	Accept *BidRecordRef `json:"accept,omitempty"`

	ClaimedBy string `json:"claimedBy,omitempty"`

	ComputeVM string `json:"computeVM,omitempty"`

	Timings BidTimings `json:"timings,omitempty"`

	Terminated *BidTerminated `json:"terminated,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	PublishAttempts int `json:"publishAttempts,omitempty"`

	LastError string `json:"lastError,omitempty"`
}

const (
	BidConditionComplete = "Complete"

	BidConditionFailed = "Failed"

	BidConditionPublished = "Published"

	BidConditionExpired = "Expired"

	BidConditionStalled = "Stalled"
)

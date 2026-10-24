package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ContractEvent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ContractEventSpec   `json:"spec,omitempty"`
	Status ContractEventStatus `json:"status,omitempty"`
}

type ContractEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ContractEvent `json:"items"`
}

type ContractEventDirection string

const (
	ContractEventOutbound ContractEventDirection = "outbound"

	ContractEventInbound ContractEventDirection = "inbound"
)

const (
	ContractEventNSIDGuestPublicKey = "com.publicdomainrelay.temp.compute.events.guest.publicKey"

	ContractEventNSIDGuestSignalWebRTC = "com.publicdomainrelay.temp.compute.events.guest.signalWebRTC"
)

type ContractEventSpec struct {
	ContractRef commonv1.LocalObjectRef `json:"contractRef"`

	Direction ContractEventDirection `json:"direction,omitempty" apigen:"default=outbound,enum=outbound|inbound"`

	NSID string `json:"nsid" apigen:"enum=com.publicdomainrelay.temp.compute.events.guest.publicKey|com.publicdomainrelay.temp.compute.events.guest.signalWebRTC"`

	Kind string `json:"kind,omitempty" apigen:"maxLength=32"`

	Tag string `json:"tag,omitempty" apigen:"pattern=^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$"`

	Key string `json:"key,omitempty" apigen:"maxLength=256"`

	Ciphertext string `json:"ciphertext,omitempty" apigen:"maxLength=16384"`
}

type ContractEventPhase string

const (
	ContractEventPending ContractEventPhase = "Pending"

	ContractEventPublished ContractEventPhase = "Published"

	ContractEventSubmitted ContractEventPhase = "Submitted"

	ContractEventReceived ContractEventPhase = "Received"

	ContractEventFailed ContractEventPhase = "Failed"
)

type ContractEventSubmit struct {
	Endpoint string `json:"endpoint,omitempty"`

	Attempts int32 `json:"attempts,omitempty"`

	LastAttemptAt string `json:"lastAttemptAt,omitempty"`

	SubmittedAt string `json:"submittedAt,omitempty"`

	LastError string `json:"lastError,omitempty"`
}

type ContractEventStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ContractEventPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Record *commonv1.StrongRef `json:"record,omitempty"`

	Event *commonv1.StrongRef `json:"event,omitempty"`

	Submit *ContractEventSubmit `json:"submit,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	ContractEventConditionPublished = "Published"

	ContractEventConditionSubmitted = "Submitted"
)

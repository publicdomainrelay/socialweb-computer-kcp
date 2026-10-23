package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type EventSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EventSourceSpec   `json:"spec,omitempty"`
	Status EventSourceStatus `json:"status,omitempty"`
}

type EventSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []EventSource `json:"items"`
}

type EventSourceSpec struct {
	BackendRef commonv1.ObjectRef `json:"backendRef"`
}

type EventSourceStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Cursor int64 `json:"cursor,omitempty"`

	LastEventAt *metav1.Time `json:"lastEventAt,omitempty"`

	Lag int64 `json:"lag,omitempty"`

	Address string `json:"address,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type PdsEventSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PdsEventSourceSpec `json:"spec,omitempty"`
	Status EventSourceStatus  `json:"status,omitempty"`
}

type PdsEventSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PdsEventSource `json:"items"`
}

type PdsEventSourceSpec struct {
	RepoRef commonv1.ObjectRef `json:"repoRef"`
}

type RelayEventSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RelayEventSourceSpec `json:"spec,omitempty"`
	Status EventSourceStatus    `json:"status,omitempty"`
}

type RelayEventSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []RelayEventSource `json:"items"`
}

type RelayEventSourceSpec struct {
	RelayRef *commonv1.ObjectRef `json:"relayRef,omitempty"`

	URL string `json:"url,omitempty" apigen:"userEndpoint"`
}

type JetstreamEventSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JetstreamEventSourceSpec `json:"spec,omitempty"`
	Status EventSourceStatus        `json:"status,omitempty"`
}

type JetstreamEventSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []JetstreamEventSource `json:"items"`
}

type JetstreamEventSourceSpec struct {
	URL string `json:"url" apigen:"userEndpoint"`
}

const (
	EventSourceConditionReady = commonv1.ConditionReady

	EventSourceConditionBackendBound = commonv1.ConditionBackendBound
)

const (
	EventSourceKind = "EventSource"

	PdsEventSourceKind = "PdsEventSource"

	RelayEventSourceKind = "RelayEventSource"

	JetstreamEventSourceKind = "JetstreamEventSource"
)

const FieldManagerEventSource = "socialweb.computer/eventsource"

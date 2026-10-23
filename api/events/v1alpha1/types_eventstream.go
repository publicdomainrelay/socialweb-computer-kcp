package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type EventStream struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EventStreamSpec   `json:"spec,omitempty"`
	Status EventStreamStatus `json:"status,omitempty"`
}

type EventStreamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []EventStream `json:"items"`
}

type EventStreamSpec struct {
	Source EventStreamSource `json:"source"`

	Cursor *int64 `json:"cursor,omitempty"`

	Sink EventStreamSink `json:"sink"`

	Suspend bool `json:"suspend,omitempty"`
}

type EventStreamSource struct {
	Kind EventStreamSourceKind `json:"kind"`

	PDSRef *commonv1.ObjectRef `json:"pdsRef,omitempty"`

	RelayRef *commonv1.ObjectRef `json:"relayRef,omitempty"`

	RelayURLs []string `json:"relayUrls,omitempty" apigen:"userEndpoint"`

	JetstreamURLs []string `json:"jetstreamUrls,omitempty" apigen:"userEndpoint"`

	Collections []string `json:"collections,omitempty"`

	DIDs []string `json:"dids,omitempty"`
}

type EventStreamSourceKind string

const (
	EventStreamSourcePDS EventStreamSourceKind = "pds"

	EventStreamSourceRelay EventStreamSourceKind = "relay"

	EventStreamSourceJetstream EventStreamSourceKind = "jetstream"
)

type EventStreamSink struct {
	Kind EventStreamSinkKind `json:"kind"`

	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`
}

type EventStreamSinkKind string

const (
	EventStreamSinkRecordWatch EventStreamSinkKind = "atprotoRecordWatch"

	EventStreamSinkHandler EventStreamSinkKind = "handler"
)

type EventStreamPhase string

const (
	EventStreamPhasePending EventStreamPhase = "Pending"

	EventStreamPhaseRunning EventStreamPhase = "Running"

	EventStreamPhaseSuspended EventStreamPhase = "Suspended"

	EventStreamPhaseFailed EventStreamPhase = "Failed"
)

const (
	EventStreamConditionReady = "Ready"

	EventStreamConditionLagging = "Lagging"

	EventStreamConditionFailed = "Failed"

	EventStreamDefaultRelayKind = "Relay"
)

type EventStreamStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase EventStreamPhase `json:"phase,omitempty"`

	Cursor int64 `json:"cursor,omitempty"`

	Lag int64 `json:"lag,omitempty"`

	LastEventAt *metav1.Time `json:"lastEventAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

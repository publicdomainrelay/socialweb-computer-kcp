package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RelayStatus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RelayStatusSpec   `json:"spec,omitempty"`
	Status RelayStatusStatus `json:"status,omitempty"`
}

type RelayStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []RelayStatus `json:"items"`
}

type RelayStatusSpec struct {
	RelayRef string `json:"relayRef"`
}

type RelayStatusPhase string

const (
	RelayStatusPhasePending RelayStatusPhase = "Pending"

	RelayStatusPhaseRunning RelayStatusPhase = "Running"

	RelayStatusPhaseFailed RelayStatusPhase = "Failed"
)

type RelayStatusStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase RelayStatusPhase `json:"phase,omitempty"`

	Cursor int64 `json:"cursor,omitempty"`

	LastEventAt string `json:"lastEventAt,omitempty"`

	Events int64 `json:"events,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type KcpInstallation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KcpInstallationSpec   `json:"spec,omitempty"`
	Status KcpInstallationStatus `json:"status,omitempty"`
}

type KcpInstallationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []KcpInstallation `json:"items"`
}

type KcpSlot string

const (
	KcpSlotA KcpSlot = "a"

	KcpSlotB KcpSlot = "b"
)

const (
	KcpInstallationConditionReady = "Ready"

	KcpInstallationConditionRolling = "Rolling"

	KcpInstallationConditionDrifted = "Drifted"

	KcpInstallationConditionLeaseStale = "LeaseStale"
)

type KcpInstallationSpec struct {
	SelectedSlot KcpSlot `json:"selectedSlot,omitempty"`

	FrontBackend string `json:"frontBackend,omitempty"`
}

type KcpInstallationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	ActiveSlot KcpSlot `json:"activeSlot,omitempty"`

	FrontBackend string `json:"frontBackend,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

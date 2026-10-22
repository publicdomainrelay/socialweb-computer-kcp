package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PlcIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlcIdentitySpec   `json:"spec,omitempty"`
	Status PlcIdentityStatus `json:"status,omitempty"`
}

type PlcIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PlcIdentity `json:"items"`
}

type PlcIdentitySpec struct {
	DID string `json:"did"`
}

type PlcIdentityStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Head string `json:"head,omitempty"`

	OpCount int64 `json:"opCount,omitempty"`

	Tombstoned bool `json:"tombstoned,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

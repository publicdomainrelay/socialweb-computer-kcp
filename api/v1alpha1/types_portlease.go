package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type PortLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PortLeaseSpec   `json:"spec,omitempty"`
	Status PortLeaseStatus `json:"status,omitempty"`
}

type PortLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PortLease `json:"items"`
}

type PortLeaseSpec struct {
	ClusterRef commonv1.ObjectRef `json:"clusterRef"`

	Purpose string `json:"purpose"`

	Port int32 `json:"port"`
}

type LeasePhase string

const (
	LeasePhasePending LeasePhase = "Pending"

	LeasePhaseAllocated LeasePhase = "Allocated"

	LeasePhaseReleased LeasePhase = "Released"
)

type PortLeaseStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase LeasePhase `json:"phase,omitempty"`

	AllocatedAt string `json:"allocatedAt,omitempty"`

	HolderRef string `json:"holderRef,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

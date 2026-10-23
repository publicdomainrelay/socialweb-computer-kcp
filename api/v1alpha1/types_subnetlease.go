package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type SubnetLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubnetLeaseSpec   `json:"spec,omitempty"`
	Status SubnetLeaseStatus `json:"status,omitempty"`
}

type SubnetLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SubnetLease `json:"items"`
}

type SubnetLeaseSpec struct {
	ClusterRef commonv1.ObjectRef `json:"clusterRef"`

	Purpose string `json:"purpose"`

	Prefix string `json:"prefix"`
}

type SubnetLeaseStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase LeasePhase `json:"phase,omitempty"`

	AllocatedAt string `json:"allocatedAt,omitempty"`

	HolderRef string `json:"holderRef,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

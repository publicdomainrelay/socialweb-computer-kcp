package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type PdsAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PdsAccountSpec   `json:"spec,omitempty"`
	Status PdsAccountStatus `json:"status,omitempty"`
}

type PdsAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PdsAccount `json:"items"`
}

type PdsAccountSpec struct {
	DID string `json:"did"`

	Handle string `json:"handle"`

	PdsRef commonv1.ObjectRef `json:"pdsRef"`
}

type PdsAccountStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Active bool `json:"active,omitempty"`

	HasPassword bool `json:"hasPassword,omitempty"`

	AppPasswordCount int32 `json:"appPasswordCount,omitempty"`

	SessionCount int32 `json:"sessionCount,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

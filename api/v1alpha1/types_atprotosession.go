package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type AtprotoSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtprotoSessionSpec   `json:"spec,omitempty"`
	Status AtprotoSessionStatus `json:"status,omitempty"`
}

type AtprotoSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtprotoSession `json:"items"`
}

type AtprotoSessionSpec struct {
	DID string `json:"did"`

	AccountRef commonv1.LocalObjectRef `json:"accountRef"`

	PdsRef commonv1.ObjectRef `json:"pdsRef"`

	SessionID string `json:"sessionId"`
}

type AtprotoSessionStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Active bool `json:"active,omitempty"`

	ExpiresAt string `json:"expiresAt,omitempty"`

	Revoked bool `json:"revoked,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

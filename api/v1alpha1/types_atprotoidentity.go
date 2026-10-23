package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type AtprotoIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtprotoIdentitySpec   `json:"spec,omitempty"`
	Status AtprotoIdentityStatus `json:"status,omitempty"`
}

type AtprotoIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtprotoIdentity `json:"items"`
}

type IdentityScope string

const (
	IdentityScopeAccount IdentityScope = "Account"

	IdentityScopeCluster IdentityScope = "Cluster"
)

type AtprotoIdentitySpec struct {
	DID string `json:"did"`

	Scope IdentityScope `json:"scope"`

	ClusterRef *commonv1.LocalObjectRef `json:"clusterRef,omitempty"`

	Revoked bool `json:"revoked,omitempty"`
}

type IdentityState string

const (
	IdentityActive IdentityState = "Active"

	IdentityRevoked IdentityState = "Revoked"

	IdentityRevocationPending IdentityState = "RevocationPending"

	IdentityColdDeferred IdentityState = "ColdDeferred"
)

type AtprotoIdentityStatus struct {
	State IdentityState `json:"state,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const FinalizerAtprotoIdentity = "atprotoidentity.socialweb.computer/revocation"

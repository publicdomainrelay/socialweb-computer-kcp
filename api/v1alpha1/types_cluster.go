package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Cluster `json:"items"`
}

type ClusterSpec struct {
	Account string `json:"account"`

	Running bool `json:"running" apigen:"optional"`
}

type ClusterPhase string

const (
	ClusterInitializing ClusterPhase = "Initializing"

	ClusterReady ClusterPhase = "Ready"

	ClusterUnavailable ClusterPhase = "Unavailable"

	ClusterDraining ClusterPhase = "Draining"
)

type ClusterStatus struct {
	Phase ClusterPhase `json:"phase,omitempty"`

	URL string `json:"URL,omitempty"`

	Instance string `json:"instance,omitempty"`

	Front *ClusterFrontStatus `json:"front,omitempty"`

	NodeExpiry *int64 `json:"nodeExpiry,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ClusterFrontStatus struct {
	Address string `json:"address,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Message string `json:"message,omitempty"`

	HeldPorts []ClusterHeldPort `json:"heldPorts,omitempty"`
}

const FieldManagerClusterFront = "socialweb.computer/cluster-front"

type ClusterHeldPort struct {
	Port int32 `json:"port"`

	Purpose string `json:"purpose,omitempty"`

	HolderRef string `json:"holderRef,omitempty"`
}

const (
	ConditionTeardownBlocked = "TeardownBlocked"

	ConditionTenantPathShut = "TenantPathShut"

	ConditionReachable = "Reachable"

	ConditionRevocationPending = "RevocationPending"

	ConditionIdentityVerificationFailed = "IdentityVerificationFailed"

	ConditionNodeProvisionBackoff = "NodeProvisionBackoff"
)

const FinalizerCluster = "cluster.socialweb.computer/teardown"

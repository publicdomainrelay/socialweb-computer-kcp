package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type Node struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeSpec   `json:"spec,omitempty"`
	Status NodeStatus `json:"status,omitempty"`
}

type NodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Node `json:"items"`
}

type NodeSpec struct {
	ClusterRef commonv1.LocalObjectRef `json:"clusterRef"`
}

type NodePhase string

const (
	NodePending NodePhase = "Pending"

	NodeRunning NodePhase = "Running"

	NodeDraining NodePhase = "Draining"
)

type NodeStatus struct {
	Phase NodePhase `json:"phase,omitempty"`

	NodeExpiry *int64 `json:"nodeExpiry,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const ConditionNodeJoined = "NodeJoined"

const FinalizerNode = "node.socialweb.computer/teardown"

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ClusterRoll struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterRollSpec   `json:"spec,omitempty"`
	Status ClusterRollStatus `json:"status,omitempty"`
}

type ClusterRollList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterRoll `json:"items"`
}

type ClusterRollSpec struct {
	Cluster string `json:"cluster"`

	Strategy string `json:"strategy"`

	MaxUnavailable int32 `json:"maxUnavailable"`
}

type ClusterRollPhase string

const (
	ClusterRollPhasePending ClusterRollPhase = "Pending"

	ClusterRollPhaseRunning ClusterRollPhase = "Running"

	ClusterRollPhaseCompleted ClusterRollPhase = "Completed"

	ClusterRollPhaseFailed ClusterRollPhase = "Failed"
)

type ClusterRollStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ClusterRollPhase `json:"phase,omitempty"`

	StartedAt string `json:"startedAt,omitempty"`

	CompletedAt string `json:"completedAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

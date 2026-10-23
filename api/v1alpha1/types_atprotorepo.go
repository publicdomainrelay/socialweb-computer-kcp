package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type AtprotoRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtprotoRepoSpec   `json:"spec,omitempty"`
	Status AtprotoRepoStatus `json:"status,omitempty"`
}

type AtprotoRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtprotoRepo `json:"items"`
}

type AtprotoRepoSpec struct {
	BackendRef commonv1.ObjectRef `json:"backendRef"`
}

type AtprotoRepoStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	DID string `json:"did,omitempty"`

	Head string `json:"head,omitempty"`

	Rev string `json:"rev,omitempty"`

	Writable bool `json:"writable,omitempty"`

	Address string `json:"address,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	AtprotoRepoConditionReady = commonv1.ConditionReady

	AtprotoRepoConditionBackendBound = commonv1.ConditionBackendBound
)

const FieldManagerAtprotoRepo = "socialweb.computer/atprotorepo"

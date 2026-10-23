package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ChildApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ChildApplicationSpec   `json:"spec,omitempty"`
	Status ChildApplicationStatus `json:"status,omitempty"`
}

type ChildApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ChildApplication `json:"items"`
}

type ChildApplicationSpec struct {
	NetworkRef commonv1.LocalObjectRef `json:"networkRef"`

	App string `json:"app"`

	Profile string `json:"profile,omitempty"`

	Placement string `json:"placement,omitempty"`
}

type ChildApplicationComponent struct {
	Kind string `json:"kind"`

	Name string `json:"name"`

	Ready bool `json:"ready"`
}

type ChildApplicationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Workspace string `json:"workspace,omitempty"`

	CoreRef string `json:"coreRef,omitempty"`

	Components []ChildApplicationComponent `json:"components,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

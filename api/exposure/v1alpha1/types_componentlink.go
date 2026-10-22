package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ComponentLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComponentLinkSpec   `json:"spec,omitempty"`
	Status ComponentLinkStatus `json:"status,omitempty"`
}

type ComponentLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ComponentLink `json:"items"`
}

type ComponentLinkSpec struct {
	From ComponentLinkEndpoint `json:"from"`

	To ComponentLinkEndpoint `json:"to"`

	Handlers []string `json:"handlers,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type ComponentLinkEndpoint struct {
	Workspace string `json:"workspace"`

	Kind string `json:"kind,omitempty"`

	Name string `json:"name,omitempty"`
}

type ComponentLinkPhase string

const (
	ComponentLinkPending ComponentLinkPhase = "Pending"

	ComponentLinkReady ComponentLinkPhase = "Ready"

	ComponentLinkFailed ComponentLinkPhase = "Failed"
)

type ComponentLinkStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ComponentLinkPhase `json:"phase,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

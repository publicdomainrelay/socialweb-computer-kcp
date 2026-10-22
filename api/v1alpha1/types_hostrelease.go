package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type HostRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostReleaseSpec   `json:"spec,omitempty"`
	Status HostReleaseStatus `json:"status,omitempty"`
}

type HostReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HostRelease `json:"items"`
}

type HostReleaseSpec struct {
	Version string `json:"version"`

	Prune bool `json:"prune,omitempty"`
}

type HostReleaseSelfCheck struct {
	Phase string `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	At string `json:"at,omitempty"`
}

type HostReleaseGC struct {
	Removed []string `json:"removed,omitempty"`

	Kept int64 `json:"kept,omitempty"`

	At string `json:"at,omitempty"`
}

type HostReleaseStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Current string `json:"current,omitempty"`

	Previous []string `json:"previous,omitempty"`

	SelfCheck *HostReleaseSelfCheck `json:"selfCheck,omitempty"`

	GC *HostReleaseGC `json:"gc,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

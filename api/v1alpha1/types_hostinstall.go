package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type HostInstall struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostInstallSpec   `json:"spec,omitempty"`
	Status HostInstallStatus `json:"status,omitempty"`
}

type HostInstallList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HostInstall `json:"items"`
}

type HostInstallSpec struct {
	Account string `json:"account"`

	Version string `json:"version"`

	Profile string `json:"profile,omitempty"`
}

type HostInstallStep struct {
	Name string `json:"name"`

	Phase string `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	At string `json:"at,omitempty"`
}

type HostInstallStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	InstalledVersion string `json:"installedVersion,omitempty"`

	Steps []HostInstallStep `json:"steps,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GuestIdentityGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GuestIdentityGrantSpec   `json:"spec,omitempty"`
	Status GuestIdentityGrantStatus `json:"status,omitempty"`
}

type GuestIdentityGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GuestIdentityGrant `json:"items"`
}

type GuestIdentityClaim struct {
	Name string `json:"name"`

	Value string `json:"value"`
}

type GuestIdentityGrantSpec struct {
	Computevm string `json:"computevm"`

	Entity string `json:"entity"`

	Api string `json:"api"`

	Actx string `json:"actx"`

	Scopes []string `json:"scopes,omitempty"`

	Claims []GuestIdentityClaim `json:"claims,omitempty"`
}

type GuestIdentityGrantStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Applied bool `json:"applied"`

	Role string `json:"role,omitempty"`

	Audience string `json:"audience,omitempty"`

	AppliedAt string `json:"appliedAt,omitempty"`

	LastError string `json:"lastError,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	GuestIdentityGrantConditionApplied = "Applied"

	GuestIdentityGrantConditionRoleConflict = "RoleConflict"

	GuestIdentityGrantConditionDrifted = "Drifted"
)

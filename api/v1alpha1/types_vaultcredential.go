package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type VaultCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VaultCredentialSpec   `json:"spec,omitempty"`
	Status VaultCredentialStatus `json:"status,omitempty"`
}

type VaultCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []VaultCredential `json:"items"`
}

type VaultCredentialSpec struct {
	commonv1.SecretRef `json:",inline"`
}

type VaultCredentialStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Exists bool `json:"exists,omitempty"`

	ExpiresAt string `json:"expiresAt,omitempty"`

	RotationDue bool `json:"rotationDue,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

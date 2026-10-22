package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type VaultCertificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VaultCertificateSpec   `json:"spec,omitempty"`
	Status VaultCertificateStatus `json:"status,omitempty"`
}

type VaultCertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []VaultCertificate `json:"items"`
}

type VaultCertificateSpec struct {
	Server string `json:"server"`

	Mount string `json:"mount"`

	Role string `json:"role"`

	CommonName string `json:"commonName"`
}

type VaultCertificateStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Exists bool `json:"exists,omitempty"`

	SerialNumber string `json:"serialNumber,omitempty"`

	Fingerprint string `json:"fingerprint,omitempty"`

	ExpiresAt string `json:"expiresAt,omitempty"`

	RotationDue bool `json:"rotationDue,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

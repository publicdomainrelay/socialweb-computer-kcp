package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ClusterClientCertificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterClientCertificateSpec   `json:"spec,omitempty"`
	Status ClusterClientCertificateStatus `json:"status,omitempty"`
}

type ClusterClientCertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterClientCertificate `json:"items"`
}

type ClusterClientCertificateSpec struct {
	ClusterRef commonv1.LocalObjectRef `json:"clusterRef"`

	Did string `json:"did"`

	Serial string `json:"serial"`

	NotAfter string `json:"notAfter"`

	NoExpiry bool `json:"noExpiry,omitempty"`

	Revoked bool `json:"revoked,omitempty"`
}

type ClusterClientCertificateStatus struct {
	RevokedAt string `json:"revokedAt,omitempty"`

	LastError string `json:"lastError,omitempty"`
}

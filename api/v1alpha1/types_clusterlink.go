package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ClusterLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterLinkSpec   `json:"spec,omitempty"`
	Status ClusterLinkStatus `json:"status,omitempty"`
}

type ClusterLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterLink `json:"items"`
}

type ClusterLinkService string

const (
	ClusterLinkApiserver ClusterLinkService = "apiserver"

	ClusterLinkOpenBao ClusterLinkService = "openbao"
)

type ClusterLinkSpec struct {
	ClusterRef commonv1.LocalObjectRef `json:"clusterRef"`

	Service ClusterLinkService `json:"service"`

	TargetPort int `json:"targetPort"`
}

type ClusterLinkPhase string

const (
	ClusterLinkPending ClusterLinkPhase = "Pending"

	ClusterLinkListening ClusterLinkPhase = "Listening"

	ClusterLinkFailed ClusterLinkPhase = "Failed"
)

type ClusterLinkStatus struct {
	Phase ClusterLinkPhase `json:"phase,omitempty"`

	EndpointID string `json:"endpointId,omitempty"`

	Did string `json:"did,omitempty"`

	Ticket string `json:"ticket,omitempty"`

	Message string `json:"message,omitempty"`
}

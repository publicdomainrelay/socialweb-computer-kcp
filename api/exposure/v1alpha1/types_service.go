package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceSpec   `json:"spec,omitempty"`
	Status ServiceStatus `json:"status,omitempty"`
}

type ServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Service `json:"items"`
}

type ServiceSpec struct {
	TargetRef ServiceTargetRef `json:"targetRef"`

	Protocol ServiceProtocol `json:"protocol"`

	Handler string `json:"handler,omitempty"`

	Auth ServiceAuth `json:"auth,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type ServiceTargetRef struct {
	Kind string `json:"kind"`

	Name string `json:"name"`

	Workspace string `json:"workspace,omitempty"`
}

type ServiceAuth struct {
	Required bool `json:"required,omitempty"`

	AllowedSubjects []string `json:"allowedSubjects,omitempty"`
}

type ServiceProtocol string

const (
	ServiceProtocolHTTP ServiceProtocol = "http"

	ServiceProtocolWS ServiceProtocol = "ws"

	ServiceProtocolTCP ServiceProtocol = "tcp"

	ServiceProtocolSSH ServiceProtocol = "ssh"
)

type ServicePhase string

const (
	ServicePending ServicePhase = "Pending"

	ServiceReady ServicePhase = "Ready"

	ServiceFailed ServicePhase = "Failed"
)

type ServiceStatus struct {
	Phase ServicePhase `json:"phase,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Endpoints []string `json:"endpoints,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

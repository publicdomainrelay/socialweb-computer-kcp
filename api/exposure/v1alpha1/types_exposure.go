package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Exposure struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExposureSpec   `json:"spec,omitempty"`
	Status ExposureStatus `json:"status,omitempty"`
}

type ExposureList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Exposure `json:"items"`
}

type ExposureSpec struct {
	ServiceRef string `json:"serviceRef"`

	Mode ExposureMode `json:"mode"`

	Hostname string `json:"hostname,omitempty"`

	Path string `json:"path,omitempty" apigen:"userEndpoint"`

	TLS ExposureTLS `json:"tls,omitempty"`

	Auth ExposureAuth `json:"auth,omitempty"`

	IngressRef string `json:"ingressRef,omitempty"`

	IngressAddress string `json:"ingressAddress,omitempty"`

	IngressDial string `json:"ingressDial,omitempty"`

	PublicHost string `json:"publicHost,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type ExposureTLS struct {
	Enabled bool `json:"enabled,omitempty"`

	CertRef string `json:"certRef,omitempty"`

	CABundle string `json:"caBundle,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`
}

type ExposureAuth struct {
	Required bool `json:"required,omitempty"`

	AllowedDIDs []string `json:"allowedDIDs,omitempty"`
}

type ExposureMode string

const (
	ExposureModeLocal ExposureMode = "local"

	ExposureModeIngress ExposureMode = "ingress"
)

type ExposurePhase string

const (
	ExposurePending ExposurePhase = "Pending"

	ExposureReady ExposurePhase = "Ready"

	ExposureFailed ExposurePhase = "Failed"
)

type ExposureStatus struct {
	Phase ExposurePhase `json:"phase,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Address string `json:"address,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

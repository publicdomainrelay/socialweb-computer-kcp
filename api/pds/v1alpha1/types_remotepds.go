package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RemotePDS struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RemotePDSSpec   `json:"spec,omitempty"`
	Status RemotePDSStatus `json:"status,omitempty"`
}

type RemotePDSList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []RemotePDS `json:"items"`
}

type RemotePDSSpec struct {
	URL string `json:"url" apigen:"userEndpoint"`

	DID string `json:"did"`

	TLS RemotePDSTLS `json:"tls,omitempty"`

	Auth RemotePDSAuth `json:"auth,omitempty"`

	Capabilities []string `json:"capabilities,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type RemotePDSTLS struct {
	Enabled bool `json:"enabled,omitempty"`

	CABundleRef string `json:"caBundleRef,omitempty"`

	ServerName string `json:"serverName,omitempty"`
}

type RemotePDSAuth struct {
	AccountRef string `json:"accountRef,omitempty"`

	AccessTokenRef string `json:"accessTokenRef,omitempty"`

	RefreshTokenRef string `json:"refreshTokenRef,omitempty"`

	DPoPKeyRef string `json:"dpopKeyRef,omitempty"`
}

type RemotePDSPhase string

const (
	RemotePDSPending RemotePDSPhase = "Pending"

	RemotePDSReady RemotePDSPhase = "Ready"

	RemotePDSFailed RemotePDSPhase = "Failed"
)

type RemotePDSStatus struct {
	Phase RemotePDSPhase `json:"phase,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`

	LastCheckedAt *metav1.Time `json:"lastCheckedAt,omitempty"`
}

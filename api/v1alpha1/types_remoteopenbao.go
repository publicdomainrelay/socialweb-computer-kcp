package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RemoteOpenBao struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RemoteOpenBaoSpec   `json:"spec,omitempty"`
	Status RemoteOpenBaoStatus `json:"status,omitempty"`
}

type RemoteOpenBaoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []RemoteOpenBao `json:"items"`
}

type RemoteOpenBaoSpec struct {
	URL string `json:"url" apigen:"userEndpoint"`

	TLS RemoteOpenBaoTLS `json:"tls,omitempty"`

	Auth RemoteOpenBaoAuth `json:"auth,omitempty"`

	Mounts RemoteOpenBaoMounts `json:"mounts,omitempty"`

	Capabilities []string `json:"capabilities,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type RemoteOpenBaoTLS struct {
	Enabled bool `json:"enabled,omitempty"`

	CABundleRef string `json:"caBundleRef,omitempty"`

	ServerName string `json:"serverName,omitempty"`
}

type RemoteOpenBaoAuthMethod string

const (
	RemoteOpenBaoAuthToken RemoteOpenBaoAuthMethod = "token"

	RemoteOpenBaoAuthAppRole RemoteOpenBaoAuthMethod = "approle"

	RemoteOpenBaoAuthCert RemoteOpenBaoAuthMethod = "cert"
)

type RemoteOpenBaoAuth struct {
	Method RemoteOpenBaoAuthMethod `json:"method"`

	TokenRef string `json:"tokenRef,omitempty"`

	AppRole *RemoteOpenBaoAppRole `json:"approle,omitempty"`

	Cert *RemoteOpenBaoCert `json:"cert,omitempty"`
}

type RemoteOpenBaoAppRole struct {
	RoleIDRef string `json:"roleIdRef"`

	SecretIDRef string `json:"secretIdRef"`
}

type RemoteOpenBaoCert struct {
	CertRef string `json:"certRef"`

	KeyRef string `json:"keyRef"`
}

type RemoteOpenBaoMounts struct {
	KV string `json:"kv,omitempty"`

	PKI string `json:"pki,omitempty"`

	Auth string `json:"auth,omitempty"`
}

type RemoteOpenBaoPhase string

const (
	RemoteOpenBaoPhasePending RemoteOpenBaoPhase = "Pending"

	RemoteOpenBaoPhaseReady RemoteOpenBaoPhase = "Ready"

	RemoteOpenBaoPhaseFailed RemoteOpenBaoPhase = "Failed"

	RemoteOpenBaoPhaseSuspended RemoteOpenBaoPhase = "Suspended"
)

type RemoteOpenBaoStatus struct {
	Phase RemoteOpenBaoPhase `json:"phase,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`

	Address string `json:"address,omitempty"`

	Initialized bool `json:"initialized,omitempty"`

	CABundle string `json:"caBundle,omitempty"`

	Version string `json:"version,omitempty"`

	Sealed bool `json:"sealed,omitempty"`

	LastCheckedAt *metav1.Time `json:"lastCheckedAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Message string `json:"message,omitempty"`
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type Vault struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VaultSpec   `json:"spec,omitempty"`
	Status VaultStatus `json:"status,omitempty"`
}

type VaultList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Vault `json:"items"`
}

type VaultSpec struct {
	BackendRef commonv1.ObjectRef `json:"backendRef"`
}

type VaultStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Address string `json:"address,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Initialized bool `json:"initialized,omitempty"`

	Sealed bool `json:"sealed,omitempty"`

	CABundle string `json:"caBundle,omitempty"`

	Version string `json:"version,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	VaultConditionReady = commonv1.ConditionReady

	VaultConditionBackendBound = commonv1.ConditionBackendBound
)

const FieldManagerVault = "socialweb.computer/vault"

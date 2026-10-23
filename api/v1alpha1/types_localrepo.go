package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type LocalRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LocalRepoSpec            `json:"spec,omitempty"`
	Status AtprotoRepoBackendStatus `json:"status,omitempty"`
}

type LocalRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []LocalRepo `json:"items"`
}

type LocalRepoSpec struct {
	DID string `json:"did"`

	PdsRef commonv1.ObjectRef `json:"pdsRef"`

	IdentityRef *commonv1.ObjectRef `json:"identityRef,omitempty"`

	AuthRef *commonv1.SecretRef `json:"authRef,omitempty"`
}

type AtprotoRepoBackendStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	DID string `json:"did,omitempty"`

	Head string `json:"head,omitempty"`

	Rev string `json:"rev,omitempty"`

	Seq int64 `json:"seq,omitempty"`

	KeyFingerprint string `json:"keyFingerprint,omitempty"`

	Writable bool `json:"writable,omitempty"`

	Address string `json:"address,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const FieldManagerLocalRepo = "socialweb.computer/localrepo"

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type AtprotoRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtprotoRepoSpec   `json:"spec,omitempty"`
	Status AtprotoRepoStatus `json:"status,omitempty"`
}

type AtprotoRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtprotoRepo `json:"items"`
}

type AtprotoRepoSpec struct {
	DID string `json:"did"`

	PdsRef *commonv1.ObjectRef `json:"pdsRef,omitempty"`

	IdentityRef *commonv1.ObjectRef `json:"identityRef,omitempty"`

	AuthRef *commonv1.SecretRef `json:"authRef,omitempty"`
}

type AtprotoRepoStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Head string `json:"head,omitempty"`

	Rev string `json:"rev,omitempty"`

	Seq int64 `json:"seq,omitempty"`

	KeyFingerprint string `json:"keyFingerprint,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

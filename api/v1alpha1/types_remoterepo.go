package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type RemoteRepo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RemoteRepoSpec           `json:"spec,omitempty"`
	Status AtprotoRepoBackendStatus `json:"status,omitempty"`
}

type RemoteRepoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []RemoteRepo `json:"items"`
}

type RemoteRepoSpec struct {
	DID string `json:"did"`

	PdsRef commonv1.ObjectRef `json:"pdsRef"`

	CredentialsRef commonv1.CredentialsRef `json:"credentialsRef"`

	AuthRef *commonv1.SecretRef `json:"authRef,omitempty"`
}

const FieldManagerRemoteRepo = "socialweb.computer/remoterepo"

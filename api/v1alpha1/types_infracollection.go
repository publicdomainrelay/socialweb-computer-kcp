package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type InfraCollection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InfraCollectionSpec   `json:"spec,omitempty"`
	Status InfraCollectionStatus `json:"status,omitempty"`
}

type InfraCollectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []InfraCollection `json:"items"`
}

type EphemeralInfraCollection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InfraCollectionSpec   `json:"spec,omitempty"`
	Status InfraCollectionStatus `json:"status,omitempty"`
}

type EphemeralInfraCollectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []EphemeralInfraCollection `json:"items"`
}

type InfraCollectionSpec struct {
	RepoRef commonv1.ObjectRef `json:"repoRef"`

	EventSourceRefs []commonv1.ObjectRef `json:"eventSourceRefs,omitempty"`
}

type InfraCollectionChild struct {
	Rkey string `json:"rkey"`

	URI string `json:"uri"`

	CID string `json:"cid,omitempty"`

	Name string `json:"name,omitempty"`

	BoundBy string `json:"boundBy,omitempty"`

	Message string `json:"message,omitempty"`
}

type InfraCollectionStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Did string `json:"did,omitempty"`

	Collection string `json:"collection,omitempty"`

	Records int `json:"records,omitempty"`

	Children []InfraCollectionChild `json:"children,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const InfraCollectionConditionListed = "Listed"

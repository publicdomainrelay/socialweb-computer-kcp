package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ComponentLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComponentLinkSpec   `json:"spec,omitempty"`
	Status ComponentLinkStatus `json:"status,omitempty"`
}

type ComponentLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ComponentLink `json:"items"`
}

type ComponentLinkSpec struct {
	From ComponentLinkFrom `json:"from"`

	To commonv1.ObjectRef `json:"to"`

	Capabilities []LinkCapability `json:"capabilities"`

	Suspend bool `json:"suspend,omitempty"`
}

type ComponentLinkFrom struct {
	Ref *commonv1.ObjectRef `json:"ref,omitempty"`

	Selector *ComponentLinkSelector `json:"selector,omitempty"`
}

type ComponentLinkSelector struct {
	Group string `json:"group,omitempty"`

	Kind string `json:"kind"`

	Workspace string `json:"workspace,omitempty"`

	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	MatchExpressions []ComponentLinkSelectorRequirement `json:"matchExpressions,omitempty"`
}

type ComponentLinkSelectorOperator string

const (
	ComponentLinkSelectorIn ComponentLinkSelectorOperator = "In"

	ComponentLinkSelectorNotIn ComponentLinkSelectorOperator = "NotIn"

	ComponentLinkSelectorExists ComponentLinkSelectorOperator = "Exists"

	ComponentLinkSelectorDoesNotExist ComponentLinkSelectorOperator = "DoesNotExist"
)

type ComponentLinkSelectorRequirement struct {
	Key string `json:"key"`

	Operator ComponentLinkSelectorOperator `json:"operator"`

	Values []string `json:"values,omitempty"`
}

type LinkCapability struct {
	Name string `json:"name"`

	Scope []string `json:"scope,omitempty"`
}

const (
	ComponentLinkConditionAccepted = "Accepted"

	ComponentLinkConditionResolvedRefs = "ResolvedRefs"
)

type ComponentLinkStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type LinkStatus struct {
	Name string `json:"name"`

	Workspace string `json:"workspace,omitempty"`

	To commonv1.ObjectRef `json:"to"`

	Capabilities []LinkCapability `json:"capabilities,omitempty"`

	Accepted bool `json:"accepted,omitempty"`

	ResolvedRefs bool `json:"resolvedRefs,omitempty"`
}

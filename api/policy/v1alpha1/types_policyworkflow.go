package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type PolicyWorkflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PolicyWorkflowSpec   `json:"spec,omitempty"`
	Status PolicyWorkflowStatus `json:"status,omitempty"`
}

type PolicyWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PolicyWorkflow `json:"items"`
}

type PolicyWorkflowSpec struct {
	EngineRef string `json:"engineRef"`

	Workflow json.RawMessage `json:"workflow"`

	RecordRef *commonv1.StrongRef `json:"recordRef,omitempty"`

	DefaultInputs map[string]string `json:"defaultInputs,omitempty"`

	DefaultArgs map[string]string `json:"defaultArgs,omitempty"`

	DefaultPerspective string `json:"defaultPerspective,omitempty"`

	DefaultSelfDid string `json:"defaultSelfDid,omitempty"`

	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`

	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

type PolicyWorkflowPhase string

const (
	PolicyWorkflowPending PolicyWorkflowPhase = "Pending"

	PolicyWorkflowReady PolicyWorkflowPhase = "Ready"

	PolicyWorkflowInvalid PolicyWorkflowPhase = "Invalid"

	PolicyWorkflowFailed PolicyWorkflowPhase = "Failed"
)

type PolicyWorkflowStatus struct {
	Phase PolicyWorkflowPhase `json:"phase,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

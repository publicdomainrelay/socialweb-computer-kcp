package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type PolicyWorkflowRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PolicyWorkflowRunSpec   `json:"spec,omitempty"`
	Status PolicyWorkflowRunStatus `json:"status,omitempty"`
}

type PolicyWorkflowRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PolicyWorkflowRun `json:"items"`
}

type PolicyWorkflowRunSpec struct {
	EngineRef string `json:"engineRef"`

	WorkflowRef string `json:"workflowRef,omitempty"`

	Workflow json.RawMessage `json:"workflow,omitempty"`

	Inputs map[string]string `json:"inputs,omitempty"`

	Args map[string]string `json:"args,omitempty"`

	Perspective string `json:"perspective,omitempty"`

	SelfDid string `json:"selfDid,omitempty"`

	SubjectDid string `json:"subjectDid,omitempty"`

	CounterpartyDid string `json:"counterpartyDid,omitempty"`

	RootRequesterDid string `json:"rootRequesterDid,omitempty"`

	RFP *commonv1.StrongRef `json:"rfp,omitempty"`

	Bid *commonv1.StrongRef `json:"bid,omitempty"`

	Accept *commonv1.StrongRef `json:"accept,omitempty"`

	Association *commonv1.AssociationPolicy `json:"association,omitempty"`

	Cache PolicyCache `json:"cache,omitempty"`

	Suspend bool `json:"suspend,omitempty"`

	Cancel bool `json:"cancel,omitempty"`

	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

type PolicyWorkflowRunPhase string

const (
	PolicyWorkflowRunPending PolicyWorkflowRunPhase = "Pending"

	PolicyWorkflowRunRunning PolicyWorkflowRunPhase = "Running"

	PolicyWorkflowRunSucceeded PolicyWorkflowRunPhase = "Succeeded"

	PolicyWorkflowRunFailed PolicyWorkflowRunPhase = "Failed"

	PolicyWorkflowRunCancelled PolicyWorkflowRunPhase = "Cancelled"
)

type PolicyExitStatus string

const (
	PolicyExitStatusSuccess PolicyExitStatus = "success"

	PolicyExitStatusFailure PolicyExitStatus = "failure"
)

type PolicyWorkflowRunStatus struct {
	Phase PolicyWorkflowRunPhase `json:"phase,omitempty"`

	RunID string `json:"runID,omitempty"`

	ExitStatus PolicyExitStatus `json:"exitStatus,omitempty"`

	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`

	Cache PolicyCache `json:"cache,omitempty"`

	ConsoleOutput string `json:"consoleOutput,omitempty"`

	Annotations map[string]json.RawMessage `json:"annotations,omitempty"`

	Restarts int32 `json:"restarts,omitempty"`

	StartTime *metav1.Time `json:"startTime,omitempty"`

	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

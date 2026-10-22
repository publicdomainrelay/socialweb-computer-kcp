package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PolicyTrigger struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PolicyTriggerSpec   `json:"spec,omitempty"`
	Status PolicyTriggerStatus `json:"status,omitempty"`
}

type PolicyTriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PolicyTrigger `json:"items"`
}

type PolicyTriggerSpec struct {
	EngineRef string `json:"engineRef"`

	WorkflowRef string `json:"workflowRef,omitempty"`

	Workflow json.RawMessage `json:"workflow,omitempty"`

	Action PolicyAction `json:"action"`

	Source PolicyTriggerSource `json:"source"`

	Match map[string]string `json:"match,omitempty"`

	RunTemplate PolicyRunTemplate `json:"runTemplate,omitempty"`

	ConcurrencyPolicy PolicyConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type PolicyAction struct {
	Kind string `json:"kind"`

	Spec json.RawMessage `json:"spec,omitempty"`
}

type PolicyTriggerSource struct {
	Type PolicyTriggerSourceType `json:"type"`

	Record *PolicyRecordTriggerSource `json:"record,omitempty"`

	Relay *PolicyRelayTriggerSource `json:"relay,omitempty"`

	Cron *PolicyCronTriggerSource `json:"cron,omitempty"`

	KCP *PolicyKCPTriggerSource `json:"kcp,omitempty"`
}

type PolicyTriggerSourceType string

const (
	PolicyTriggerSourceRecord PolicyTriggerSourceType = "Record"

	PolicyTriggerSourceRelay PolicyTriggerSourceType = "Relay"

	PolicyTriggerSourceCron PolicyTriggerSourceType = "Cron"

	PolicyTriggerSourceKCP PolicyTriggerSourceType = "KCP"
)

type PolicyRecordTriggerSource struct {
	Repo string `json:"repo,omitempty"`

	Collection string `json:"collection,omitempty"`
}

type PolicyRelayTriggerSource struct {
	Relay string `json:"relay,omitempty"`

	Collection string `json:"collection,omitempty"`
}

type PolicyCronTriggerSource struct {
	Schedule string `json:"schedule"`
}

type PolicyKCPTriggerSource struct {
	Resource string `json:"resource"`

	LabelSelector string `json:"labelSelector,omitempty"`
}

type PolicyRunTemplate struct {
	Inputs map[string]string `json:"inputs,omitempty"`

	Args map[string]string `json:"args,omitempty"`

	Perspective string `json:"perspective,omitempty"`

	SelfDid string `json:"selfDid,omitempty"`

	SubjectDid string `json:"subjectDid,omitempty"`

	CounterpartyDid string `json:"counterpartyDid,omitempty"`

	RootRequesterDid string `json:"rootRequesterDid,omitempty"`

	RFP *PolicyStrongRef `json:"rfp,omitempty"`

	Bid *PolicyStrongRef `json:"bid,omitempty"`

	Accept *PolicyStrongRef `json:"accept,omitempty"`

	Cache PolicyCache `json:"cache,omitempty"`

	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

type PolicyConcurrencyPolicy string

const (
	PolicyConcurrencyAllow PolicyConcurrencyPolicy = "Allow"

	PolicyConcurrencyForbid PolicyConcurrencyPolicy = "Forbid"

	PolicyConcurrencyReplace PolicyConcurrencyPolicy = "Replace"
)

type PolicyTriggerStatus struct {
	LastRun *metav1.Time `json:"lastRun,omitempty"`

	NextRun *metav1.Time `json:"nextRun,omitempty"`

	Active int32 `json:"active,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

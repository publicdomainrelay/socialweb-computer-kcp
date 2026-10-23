package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type StateSync struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StateSyncSpec   `json:"spec,omitempty"`
	Status StateSyncStatus `json:"status,omitempty"`
}

type StateSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []StateSync `json:"items"`
}

type StateSyncSource struct {
	Workspace string `json:"workspace,omitempty"`

	ApiVersion string `json:"apiVersion,omitempty"`

	Kind string `json:"kind,omitempty"`

	Name string `json:"name,omitempty"`

	UID string `json:"uid,omitempty"`

	Generation int64 `json:"generation,omitempty"`
}

type StateSyncSink struct {
	Adapter string `json:"adapter"`

	Workspace string `json:"workspace,omitempty"`

	Collection string `json:"collection,omitempty"`

	Target string `json:"target,omitempty"`

	SubmitEventURL string `json:"submitEventUrl,omitempty" apigen:"userEndpoint"`
}

type StateSyncDirection string

const (
	StateSyncDirectionOutbound StateSyncDirection = "Outbound"

	StateSyncDirectionInbound StateSyncDirection = "Inbound"

	StateSyncDirectionBoth StateSyncDirection = "Both"
)

type StateSyncConflictPolicy string

const (
	StateSyncConflictSourceWins StateSyncConflictPolicy = "SourceWins"

	StateSyncConflictTargetWins StateSyncConflictPolicy = "TargetWins"

	StateSyncConflictReportOnly StateSyncConflictPolicy = "ReportOnly"
)

type StateSyncDeletionPolicy string

const (
	StateSyncDeletionRetain StateSyncDeletionPolicy = "Retain"

	StateSyncDeletionDeleteRemote StateSyncDeletionPolicy = "DeleteRemote"

	StateSyncDeletionAckThenDelete StateSyncDeletionPolicy = "AckThenDelete"
)

type StateSyncAck struct {
	Paths []string `json:"paths,omitempty" apigen:"userEndpoint"`

	FieldManager string `json:"fieldManager,omitempty"`
}

type StateSyncRetry struct {
	MaxAttempts int `json:"maxAttempts,omitempty"`

	BackoffSeconds int `json:"backoffSeconds,omitempty"`
}

type StateSyncSpec struct {
	Source *StateSyncSource `json:"source,omitempty"`

	Sink StateSyncSink `json:"sink"`

	Direction StateSyncDirection `json:"direction,omitempty"`

	Payload json.RawMessage `json:"payload,omitempty"`

	IdempotencyKey string `json:"idempotencyKey"`

	ConflictPolicy StateSyncConflictPolicy `json:"conflictPolicy,omitempty"`

	DeletionPolicy StateSyncDeletionPolicy `json:"deletionPolicy,omitempty"`

	Ack *StateSyncAck `json:"ack,omitempty"`

	Retry *StateSyncRetry `json:"retry,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type StateSyncPhase string

const (
	StateSyncPhasePending StateSyncPhase = "Pending"

	StateSyncPhaseDelivering StateSyncPhase = "Delivering"

	StateSyncPhaseAcked StateSyncPhase = "Acked"

	StateSyncPhaseDrifted StateSyncPhase = "Drifted"

	StateSyncPhaseFailed StateSyncPhase = "Failed"

	StateSyncPhaseStalled StateSyncPhase = "Stalled"
)

type StateSyncObservation struct {
	Exists bool `json:"exists"`

	Ref string `json:"ref,omitempty"`

	Cid string `json:"cid,omitempty"`

	Hash string `json:"hash,omitempty"`

	At string `json:"at,omitempty"`

	Meta map[string]string `json:"meta,omitempty"`
}

type StateSyncAcknowledged struct {
	Ref string `json:"ref,omitempty"`

	Cid string `json:"cid,omitempty"`

	At string `json:"at,omitempty"`

	EventURI string `json:"eventUri,omitempty"`

	EventCid string `json:"eventCid,omitempty"`
}

type StateSyncStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase StateSyncPhase `json:"phase,omitempty"`

	DesiredHash string `json:"desiredHash,omitempty"`

	Observed *StateSyncObservation `json:"observed,omitempty"`

	Acked *StateSyncAcknowledged `json:"acked,omitempty"`

	Cursor *int64 `json:"cursor,omitempty"`

	Attempts int `json:"attempts,omitempty"`

	LastAttemptAt string `json:"lastAttemptAt,omitempty"`

	LastError string `json:"lastError,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	StateSyncConditionAcked = "Acked"

	StateSyncConditionObserved = "Observed"

	StateSyncConditionDrifted = "Drifted"

	StateSyncConditionStalled = "Stalled"
)

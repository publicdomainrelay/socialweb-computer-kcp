package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type AtprotoRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtprotoRecordSpec   `json:"spec,omitempty"`
	Status AtprotoRecordStatus `json:"status,omitempty"`
}

type AtprotoRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtprotoRecord `json:"items"`
}

type AtprotoRecordSpec struct {
	RepoRef *commonv1.ObjectRef `json:"repoRef,omitempty"`

	Collection string `json:"collection"`

	Rkey string `json:"rkey"`

	Record json.RawMessage `json:"record"`

	OnMissingReference MissingReferencePolicy `json:"onMissingReference,omitempty"`

	SwapCid string `json:"swapCid,omitempty"`

	DeletionPolicy RecordDeletionPolicy `json:"deletionPolicy,omitempty" apigen:"default=Delete,enum=Delete|Retain"`
}

type RecordDeletionPolicy string

const (
	RecordDeletionDelete RecordDeletionPolicy = "Delete"

	RecordDeletionRetain RecordDeletionPolicy = "Retain"
)

type MissingReferencePolicy string

const (
	MissingReferenceKeep MissingReferencePolicy = "Keep"

	MissingReferenceDelete MissingReferencePolicy = "Delete"
)

type AtprotoRecordPhase string

const (
	AtprotoRecordPhasePending AtprotoRecordPhase = "Pending"

	AtprotoRecordPhaseApplied AtprotoRecordPhase = "Applied"

	AtprotoRecordPhaseFailed AtprotoRecordPhase = "Failed"
)

type AtprotoRecordStatus struct {
	Phase AtprotoRecordPhase `json:"phase,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Did string `json:"did,omitempty"`

	URI string `json:"uri,omitempty"`

	Cid string `json:"cid,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const ConditionReferencesResolved = "ReferencesResolved"

const ConditionRecordInSync = "InSync"

const FinalizerAtprotoRecord = "atprotorecord.socialweb.computer/repo"

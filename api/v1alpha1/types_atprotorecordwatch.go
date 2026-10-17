package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AtprotoRecordWatch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtprotoRecordWatchSpec   `json:"spec,omitempty"`
	Status AtprotoRecordWatchStatus `json:"status,omitempty"`
}

type AtprotoRecordWatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtprotoRecordWatch `json:"items"`
}

type AtprotoRecordWatchSpec struct {
	Repo string `json:"repo"`

	Collection string `json:"collection"`

	Match []AtprotoRecordMatch `json:"match,omitempty"`

	Source AtprotoRecordWatchSource `json:"source"`
}

type AtprotoRecordMatch struct {
	FieldPath string `json:"fieldPath"`

	Value string `json:"value,omitempty"`

	ValueFrom *AtprotoValueFrom `json:"valueFrom,omitempty"`
}

type AtprotoValueFrom struct {
	Kind string `json:"kind,omitempty"`

	Resource string `json:"resource,omitempty"`

	ApiVersion string `json:"apiVersion,omitempty"`

	Name string `json:"name"`

	FieldPath string `json:"fieldPath"`

	Workspace string `json:"workspace,omitempty"`

	Namespace string `json:"namespace,omitempty"`
}

type AtprotoRecordWatchSource struct {
	RelayUrls []string `json:"relayUrls,omitempty"`

	JetstreamUrls []string `json:"jetstreamUrls,omitempty"`
}

type WatchedRecord struct {
	Did string `json:"did"`

	Handle string `json:"handle,omitempty"`

	Uri string `json:"uri"`

	Cid string `json:"cid,omitempty"`

	Value json.RawMessage `json:"value"`

	VerifiedAt string `json:"verifiedAt"`
}

type AtprotoRecordWatchCursor struct {
	Source string `json:"source"`

	Cursor int64 `json:"cursor"`
}

type AtprotoRecordWatchStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Records []WatchedRecord `json:"records,omitempty"`

	Cursors []AtprotoRecordWatchCursor `json:"cursors,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const ConditionVerified = "Verified"

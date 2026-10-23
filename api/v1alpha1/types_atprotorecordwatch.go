package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
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
	RepoRef *commonv1.ObjectRef `json:"repoRef,omitempty"`

	Repo string `json:"repo,omitempty"`

	Collection string `json:"collection"`

	Match []AtprotoRecordMatch `json:"match,omitempty"`

	Source AtprotoRecordWatchSource `json:"source"`
}

type AtprotoRecordMatch struct {
	FieldPath string `json:"fieldPath" apigen:"userEndpoint"`

	Value string `json:"value,omitempty"`

	ValueFrom *AtprotoValueFrom `json:"valueFrom,omitempty"`
}

type AtprotoValueFrom struct {
	Kind string `json:"kind,omitempty"`

	Resource string `json:"resource,omitempty"`

	ApiVersion string `json:"apiVersion,omitempty"`

	Name string `json:"name"`

	FieldPath string `json:"fieldPath" apigen:"userEndpoint"`

	Workspace string `json:"workspace,omitempty"`

	Namespace string `json:"namespace,omitempty"`
}

type AtprotoRecordWatchSource struct {
	RelayURLs []string `json:"relayUrls,omitempty" apigen:"userEndpoint"`

	JetstreamURLs []string `json:"jetstreamUrls,omitempty" apigen:"userEndpoint"`
}

type WatchedRecord struct {
	Did string `json:"did"`

	Handle string `json:"handle,omitempty"`

	URI string `json:"uri"`

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

	Deleted []string `json:"deleted,omitempty"`

	Cursors []AtprotoRecordWatchCursor `json:"cursors,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const ConditionVerified = "Verified"

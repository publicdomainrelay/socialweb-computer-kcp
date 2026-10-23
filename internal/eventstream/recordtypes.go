package eventstream

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type recordWatchSpec struct {
	RepoRef *recordRepoRef `json:"repoRef,omitempty"`

	Repo string `json:"repo"`

	Collection string `json:"collection"`

	Match []recordMatch `json:"match,omitempty"`

	Source recordWatchSource `json:"source"`
}

type recordRepoRef struct {
	Workspace string `json:"workspace,omitempty"`

	Kind string `json:"kind,omitempty"`

	Name string `json:"name"`
}

type recordMatch struct {
	FieldPath string `json:"fieldPath"`

	Value string `json:"value,omitempty"`

	ValueFrom *recordValueFrom `json:"valueFrom,omitempty"`
}

type recordValueFrom struct {
	Kind string `json:"kind,omitempty"`

	Resource string `json:"resource,omitempty"`

	ApiVersion string `json:"apiVersion,omitempty"`

	Name string `json:"name"`

	FieldPath string `json:"fieldPath"`

	Workspace string `json:"workspace,omitempty"`

	Namespace string `json:"namespace,omitempty"`
}

type recordWatchSource struct {
	EventSourceRefs []commonv1.ObjectRef `json:"eventSourceRefs,omitempty"`
}

type recordWatchCursor struct {
	Source string `json:"source"`

	Cursor int64 `json:"cursor"`
}

type watchedRecord struct {
	Did string `json:"did"`

	Handle string `json:"handle,omitempty"`

	Uri string `json:"uri"`

	Cid string `json:"cid,omitempty"`

	Value json.RawMessage `json:"value"`

	VerifiedAt string `json:"verifiedAt"`
}

type recordWatchStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Records []watchedRecord `json:"records,omitempty"`

	Cursors []recordWatchCursor `json:"cursors,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

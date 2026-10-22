package eventstream

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type recordWatchSpec struct {
	Repo string `json:"repo"`

	Collection string `json:"collection"`
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

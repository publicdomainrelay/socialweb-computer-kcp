package infra

import (
	"maps"
	"reflect"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func metaChanged(old, new metav1.Object) bool {
	return old.GetGeneration() != new.GetGeneration() || !maps.Equal(old.GetLabels(), new.GetLabels()) ||
		old.GetDeletionTimestamp().IsZero() != new.GetDeletionTimestamp().IsZero() ||
		!reflect.DeepEqual(old.GetOwnerReferences(), new.GetOwnerReferences())
}

type contractView struct {
	phase v1alpha1.ComputeContractPhase

	message string

	receipt string

	winner string
}

func viewOfContract(cc *v1alpha1.ComputeContract) contractView {
	out := contractView{phase: cc.Status.Phase, message: cc.Status.Message}
	if state := cc.Status.Contract; state != nil {
		out.receipt = state.ReceiptURI
		if state.Winner != nil {
			out.winner = state.Winner.DID
		}
	}
	return out
}

func contractChanged(old, new *v1alpha1.ComputeContract) bool {
	return metaChanged(old, new) || viewOfContract(old) != viewOfContract(new)
}

type recordAt struct {
	uri string

	cid string
}

func watchedRecords(w *v1alpha1.AtprotoRecordWatch) []recordAt {
	out := make([]recordAt, 0, len(w.Status.Records))
	for _, rec := range w.Status.Records {
		out = append(out, recordAt{uri: rec.URI, cid: rec.Cid})
	}
	return out
}

func recordWatchChanged(old, new *v1alpha1.AtprotoRecordWatch) bool {
	return metaChanged(old, new) || !slices.Equal(watchedRecords(old), watchedRecords(new)) || !slices.Equal(old.Status.Deleted, new.Status.Deleted)
}

func linkAccepted(link *exposurev1alpha1.ComponentLink) bool {
	if link.Status.ObservedGeneration < link.Generation {
		return false
	}
	return meta.IsStatusConditionTrue(link.Status.Conditions, exposurev1alpha1.ComponentLinkConditionAccepted)
}

func linkChanged(old, new *exposurev1alpha1.ComponentLink) bool {
	return metaChanged(old, new) || old.Status.ObservedGeneration != new.Status.ObservedGeneration || linkAccepted(old) != linkAccepted(new)
}

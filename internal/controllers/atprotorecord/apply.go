package atprotorecord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

type References struct {
	Own func(ctx context.Context, from v1alpha1.AtprotoValueFrom) (map[string]any, bool, error)

	Dynamic *kube.Client

	Factory DynamicFactory

	Workspace string
}

type Outcome struct {
	Phase v1alpha1.AtprotoRecordPhase

	Did string

	URI string

	CID string

	Message string

	Reason string

	Written bool

	Retry bool

	Conditions []metav1.Condition
}

func (o Outcome) Status(object *v1alpha1.AtprotoRecord) v1alpha1.AtprotoRecordStatus {
	status := v1alpha1.AtprotoRecordStatus{Phase: o.Phase, ObservedGeneration: object.Generation, Did: o.Did, URI: o.URI, Cid: o.CID,
		Message: o.Message, Conditions: slices.Clone(object.Status.Conditions)}
	for _, condition := range o.Conditions {
		conds.Set(&status.Conditions, condition.ObservedGeneration, condition.Type, condition.Status == metav1.ConditionTrue,
			condition.Reason, condition.Message)
	}
	return status
}

func failed(object *v1alpha1.AtprotoRecord, reason string, cause error) Outcome {
	message := cause.Error()
	generation := object.Generation
	return Outcome{Phase: v1alpha1.AtprotoRecordPhaseFailed, Did: object.Status.Did, URI: object.Status.URI, CID: object.Status.Cid,
		Message: message, Reason: reason, Retry: reason != ReasonInvalidSpec && reason != ReasonRepoNotReady,
		Conditions: []metav1.Condition{
			conds.New(ConditionReferencesResolved, reason != ReasonInvalidSpec, reason, message, generation),
			conds.New(ConditionInSync, false, reason, message, generation),
		}}
}

func Apply(ctx context.Context, repo atprotorepo.Repo, object *v1alpha1.AtprotoRecord, refs References) (Outcome, error) {
	spec := object.Spec
	generation := object.Generation
	did := repo.DID()
	collection, rkey := strings.TrimSpace(spec.Collection), strings.TrimSpace(spec.Rkey)
	if collection == "" || rkey == "" {
		err := errors.New("the record needs a collection and an rkey")
		return failed(object, ReasonInvalidSpec, err), err
	}
	resolved, missing, err := resolveReferences(ctx, refs, spec.Record)
	if err != nil {
		return failed(object, ReasonInvalidSpec, err), err
	}
	if len(missing) > 0 {
		message := "the record references: " + strings.Join(missing, "; ")
		out := Outcome{Phase: v1alpha1.AtprotoRecordPhasePending, Did: did, Message: message, Reason: "MissingReference"}
		if spec.OnMissingReference == v1alpha1.MissingReferenceDelete {
			if err := deleteRecord(ctx, repo, collection, rkey); err != nil {
				return failed(object, "DeleteFailed", err), err
			}
			out.Conditions = []metav1.Condition{
				conds.New(ConditionReferencesResolved, false, "MissingReference", message, generation),
				conds.New(ConditionInSync, true, "Deleted", "the record is absent because a reference is missing", generation),
			}
			return out, nil
		}
		out.Conditions = []metav1.Condition{
			conds.New(ConditionReferencesResolved, false, "MissingReference", message, generation),
			conds.New(ConditionInSync, false, "MissingReference", "a reference is missing", generation),
		}
		return out, nil
	}
	if resolved, err = typed(resolved, collection); err != nil {
		return failed(object, ReasonInvalidSpec, err), err
	}
	held, err := repo.GetRecord(ctx, did, collection, rkey)
	absent := errors.Is(err, atprotorepo.ErrNotFound)
	if errors.Is(err, atprotorepo.ErrNotBound) {
		return failed(object, ReasonRepoNotReady, err), nil
	}
	if err != nil && !absent {
		return failed(object, "ObserveFailed", err), err
	}
	out := Outcome{Phase: v1alpha1.AtprotoRecordPhaseApplied, Did: did, URI: held.URI, CID: held.CID, Reason: "InSync"}
	if swap := strings.TrimSpace(spec.SwapCid); swap != "" && (absent || held.CID != swap) && (absent || !recordsEqual(held.Value, resolved)) {
		message := "the repo holds cid " + held.CID + ", not the swapCid " + swap + "; the record is not written"
		if absent {
			message = "the repo holds no record, not the swapCid " + swap + "; the record is not written"
		}
		out.Phase, out.Reason, out.Message = v1alpha1.AtprotoRecordPhasePending, ReasonSwapMismatch, message
		out.Conditions = []metav1.Condition{
			conds.New(ConditionReferencesResolved, true, "Resolved", "every reference is resolved", generation),
			conds.New(ConditionInSync, false, ReasonSwapMismatch, message, generation),
		}
		return out, nil
	}
	switch {
	case absent:
		out.Reason = "Created"
	case !recordsEqual(held.Value, resolved):
		out.Reason = "Repaired"
		out.Message = "the repo held cid " + held.CID + "; the declared record was written"
	}
	if out.Reason != "InSync" {
		written, err := repo.PutRecord(ctx, collection, rkey, resolved)
		if err != nil {
			return failed(object, "WriteFailed", err), err
		}
		out.URI, out.CID, out.Written = written.URI, written.CID, true
	}
	out.Conditions = []metav1.Condition{
		conds.New(ConditionReferencesResolved, true, "Resolved", "every reference is resolved", generation),
		conds.New(ConditionInSync, true, out.Reason, "the repo holds the declared record", generation),
	}
	return out, nil
}

func deleteRecord(ctx context.Context, repo atprotorepo.RecordWriter, collection, rkey string) error {
	if err := repo.DeleteRecord(ctx, collection, rkey); err != nil && !errors.Is(err, atprotorepo.ErrNotFound) {
		return err
	}
	return nil
}

func typed(raw json.RawMessage, collection string) (json.RawMessage, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, fmt.Errorf("the record is not a JSON object")
	}
	if _, ok := value["$type"]; ok {
		return raw, nil
	}
	value["$type"] = collection
	return json.Marshal(value)
}

func recordsEqual(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

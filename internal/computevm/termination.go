package computevm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	TerminationSyncAdapter = "atproto.market.event"

	TerminationSyncSourceLabel = "statesync.socialweb.computer/source"

	TerminationSyncUIDLabel = "statesync.socialweb.computer/source-uid"

	TerminationSyncVMNameLabel = "statesync.socialweb.computer/computevm-name"

	TerminationSyncSource = "ComputeVM"

	TerminationSyncPrefix = "termination-"
)

func TerminationSyncName(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return TerminationSyncPrefix + hex.EncodeToString(sum[:8])
}

func TerminationStateSync(o *v1alpha1.ComputeVM, workspace, reason, observedAt string, request *commonv1.StrongRef) (*unstructured.Unstructured, error) {
	if o == nil || o.Spec.Contract == nil {
		return nil, errors.New("computevm: a termination StateSync needs a contract")
	}
	contract := o.Spec.Contract
	if contract.Receipt.URI == "" || contract.Receipt.CID == "" {
		return nil, errors.New("computevm: a termination StateSync needs a receipt")
	}
	if reason == "" || observedAt == "" {
		return nil, errors.New("computevm: a termination StateSync needs a reason and an observed time")
	}
	marker := terminationMarker(o)
	if request == nil && marker != nil {
		request = marker.Request
	}
	record := map[string]any{
		"$type":      market.ComputeEventsVMTerminatedNSID,
		"reason":     reason,
		"observedAt": observedAt,
		"createdAt":  observedAt,
	}
	if marker != nil && marker.ExitCode != nil {
		record["exitCode"] = *marker.ExitCode
	}
	if request != nil && request.URI != "" {
		record["request"] = market.NewStrongRef(request.URI, request.CID).Map()
	}
	payload := map[string]any{
		"collection": market.ComputeEventsVMTerminatedNSID,
		"receipt":    map[string]any{"uri": contract.Receipt.URI, "cid": contract.Receipt.CID},
		"record":     record,
	}
	if contract.SubmitEventURL != "" {
		payload["submitEventUrl"] = contract.SubmitEventURL
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	metadata := map[string]any{
		"name": TerminationSyncName(string(o.UID)),
		"labels": map[string]any{
			TerminationSyncSourceLabel: TerminationSyncSource,
			TerminationSyncUIDLabel:    string(o.UID),
			TerminationSyncVMNameLabel: o.Name,
		},
	}
	for _, owner := range o.OwnerReferences {
		if owner.Kind != "Bid" || owner.UID == "" {
			continue
		}
		metadata["ownerReferences"] = []any{map[string]any{
			"apiVersion": owner.APIVersion,
			"kind":       owner.Kind,
			"name":       owner.Name,
			"uid":        string(owner.UID),
		}}
		break
	}
	object := map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "StateSync",
		"metadata":   metadata,
		"spec": map[string]any{
			"source": map[string]any{
				"workspace":  workspace,
				"apiVersion": v1alpha1.GroupVersion.String(),
				"kind":       "ComputeVM",
				"name":       o.Name,
				"uid":        string(o.UID),
			},
			"sink":           map[string]any{"adapter": TerminationSyncAdapter},
			"direction":      string(v1alpha1.StateSyncDirectionOutbound),
			"payload":        stored,
			"idempotencyKey": "terminated:" + contract.Receipt.URI,
			"conflictPolicy": string(v1alpha1.StateSyncConflictSourceWins),
			"deletionPolicy": string(v1alpha1.StateSyncDeletionRetain),
		},
	}
	return &unstructured.Unstructured{Object: object}, nil
}

func terminationMarker(o *v1alpha1.ComputeVM) *v1alpha1.ComputeVMTermination {
	if o == nil || o.Status.Market == nil {
		return nil
	}
	return o.Status.Market.Termination
}

func (ct *Controller) ensureTerminationSync(ctx context.Context, o v1alpha1.ComputeVM, reason, observedAt string, request *commonv1.StrongRef) error {
	if !terminationReportable(o) {
		return nil
	}
	object, err := TerminationStateSync(&o, ct.c.Workspace, reason, observedAt, request)
	if err != nil {
		return err
	}
	name := object.GetName()
	if _, err := ct.syncs.Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("computevm: creating the termination StateSync %s: %w", name, err)
	}
	ct.c.log().Info("a termination StateSync is created", "computevm", o.Name, "statesync", name)
	return nil
}

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

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"

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

func TerminationStateSync(o *v1alpha1.ComputeVM, workspace, reason, observedAt string, request *commonv1.StrongRef) (*v1alpha1.StateSync, error) {
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
	if request == nil {
		request = ReleaseRequestRef(o)
	}
	record := map[string]any{
		"$type":      market.ComputeEventsVMTerminatedNSID,
		"reason":     reason,
		"observedAt": observedAt,
		"createdAt":  observedAt,
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
	sync := &v1alpha1.StateSync{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "StateSync"},
		ObjectMeta: metav1.ObjectMeta{Name: TerminationSyncName(string(o.UID)), Labels: map[string]string{
			TerminationSyncSourceLabel: TerminationSyncSource,
			TerminationSyncUIDLabel:    string(o.UID),
			TerminationSyncVMNameLabel: o.Name,
		}},
		Spec: v1alpha1.StateSyncSpec{
			Source: &v1alpha1.StateSyncSource{Workspace: workspace, ApiVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM",
				Name: o.Name, UID: string(o.UID)},
			Sink:           v1alpha1.StateSyncSink{Adapter: TerminationSyncAdapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        raw,
			IdempotencyKey: "terminated:" + contract.Receipt.URI,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
	for _, owner := range o.OwnerReferences {
		if owner.Kind != "Bid" || owner.UID == "" {
			continue
		}
		sync.OwnerReferences = []metav1.OwnerReference{{APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: owner.UID}}
		break
	}
	return sync, nil
}

func TerminationReportable(o v1alpha1.ComputeVM) bool {
	contract := o.Spec.Contract
	return contract != nil && contract.Receipt.URI != "" && contract.Receipt.CID != ""
}

func (h *Host) EnsureTerminationSync(ctx context.Context, o v1alpha1.ComputeVM, reason, observedAt string, request *commonv1.StrongRef) error {
	if !TerminationReportable(o) {
		return nil
	}
	object, err := TerminationStateSync(&o, h.c.Workspace, reason, observedAt, request)
	if err != nil {
		return err
	}
	name := object.Name
	if _, err := kube.Create[v1alpha1.StateSync](ctx, h.syncs, object, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("computevm: creating the termination StateSync %s: %w", name, err)
	}
	h.c.log().Info("a termination StateSync is created", "computevm", o.Name, "statesync", name)
	return nil
}

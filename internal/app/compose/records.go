package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecord"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

const (
	VouchNSID = "sh.tangled.graph.vouch"

	OperatorRepoName = "operator"

	OperatorVouchPrefix = "operator-vouch-"
)

type OwnerSession struct {
	Service    string `json:"service"`
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

func ReadOwnerSessions(path string) (map[string]OwnerSession, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sessions map[string]OwnerSession
	if err := json.Unmarshal(b, &sessions); err != nil {
		return nil, fmt.Errorf("compose: the owner sessions file %s: %w", path, err)
	}
	return sessions, nil
}

func FirstOwnerSession(path string) (string, OwnerSession, error) {
	sessions, err := ReadOwnerSessions(path)
	if err != nil {
		return "", OwnerSession{}, err
	}
	owners := make([]string, 0, len(sessions))
	for did := range sessions {
		owners = append(owners, did)
	}
	if len(owners) == 0 {
		return "", OwnerSession{}, fmt.Errorf("compose: the owner sessions file %s holds no session", path)
	}
	slices.Sort(owners)
	return owners[0], sessions[owners[0]], nil
}

func (c *Composer) ApplyOperatorVouch(ctx context.Context, r Root, ws, app, subject, at string) error {
	record, err := json.Marshal(map[string]any{"$type": VouchNSID, "subject": subject, "createdAt": at})
	if err != nil {
		return err
	}
	obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: OperatorVouchPrefix + app},
		Spec: v1alpha1.AtprotoRecordSpec{Repo: OperatorRepoName, Collection: VouchNSID, Rkey: subject, Record: record}}
	return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
}

func (c *Composer) OperatorRecords(ctx context.Context, r Root, ws string, op Operator) error {
	ri, err := c.Client(ws, AtprotoRecordGVR)
	if err != nil {
		return err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var records []v1alpha1.AtprotoRecord
	for i := range list.Items {
		var o v1alpha1.AtprotoRecord
		if err := unstruct.Into(&list.Items[i], &o); err != nil {
			return err
		}
		if o.Spec.Repo == OperatorRepoName && o.Spec.RepoRef == nil && o.DeletionTimestamp == nil &&
			(o.Status.Phase != v1alpha1.AtprotoRecordPhaseApplied || o.Status.ObservedGeneration != o.Generation) {
			records = append(records, o)
		}
	}
	if len(records) == 0 {
		return nil
	}
	repo, _, err := c.OperatorRepo(ctx, r, op)
	if err != nil {
		return err
	}
	for i := range records {
		o := &records[i]
		outcome, applyErr := atprotorecord.Apply(ctx, repo, o, atprotorecord.References{Workspace: ws})
		status := outcome.Status(o)
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
		if err != nil {
			return err
		}
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AtprotoRecord", "status": m}}
		u.SetName(o.Name)
		body, err := u.MarshalJSON()
		if err != nil {
			return err
		}
		force := true
		if _, err := ri.Patch(ctx, o.Name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: r.Manager() + "/records", Force: &force}, "status"); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("AtprotoRecord %s: writing the status: %w", o.Name, err)
		}
		if applyErr != nil {
			return fmt.Errorf("AtprotoRecord %s: %w", o.Name, applyErr)
		}
	}
	return nil
}

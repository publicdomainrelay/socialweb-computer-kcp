package recordcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	FieldManager = "atproto-record-controller"

	ConditionReferencesResolved = v1alpha1.ConditionReferencesResolved

	ConditionInSync = v1alpha1.ConditionRecordInSync
)

var AtprotoRecordGVR = v1alpha1.AtprotoRecordGVR

var AtprotoRepoGVR = v1alpha1.AtprotoRepoGVR

var RemotePDSGVR = pdsv1alpha1.RemotePDSGVR

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type Options struct {
	Resolver *pdsclient.RepoResolver

	Dynamic DynamicFactory

	Log *slog.Logger

	AddFinalizer bool
}

type Controller struct {
	resolver *pdsclient.RepoResolver

	dynamic DynamicFactory

	log *slog.Logger

	addFinalizer bool
}

func New(opts Options) (*Controller, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{resolver: opts.Resolver, dynamic: opts.Dynamic, log: log.With("component", "atproto-record-controller"),
		addFinalizer: opts.AddFinalizer}, nil
}

type repoTarget struct {
	pds pdsclient.PDS

	did syntax.DID
}

func (c *Controller) Reconcile(ctx context.Context, dyn dynamic.Interface, workspace string, u *unstructured.Unstructured) error {
	if u == nil {
		return nil
	}
	object, err := decodeRecord(u)
	if err != nil {
		return err
	}
	if object.DeletionTimestamp == nil && object.Spec.RepoRef == nil && strings.TrimSpace(object.Spec.Repo) == "operator" {
		return nil
	}
	ri := dyn.Resource(AtprotoRecordGVR)
	if object.DeletionTimestamp != nil {
		return c.reconcileDelete(ctx, ri, dyn, workspace, u, nil)
	}
	target, err := c.resolveRepoTarget(ctx, dyn, workspace, object)
	if err != nil {
		return c.writeFailure(ctx, ri, u, object, "RepoRef", err)
	}
	return c.reconcileObject(ctx, ri, dyn, workspace, u, object, target, c.addFinalizer)
}

func (c *Controller) ReconcileWithPDS(ctx context.Context, ri dynamic.ResourceInterface, u *unstructured.Unstructured,
	pds pdsclient.PDS, did syntax.DID) error {
	if u == nil || ri == nil {
		return nil
	}
	object, err := decodeRecord(u)
	if err != nil {
		return err
	}
	if object.DeletionTimestamp != nil {
		return c.reconcileDelete(ctx, ri, nil, "", u, &repoTarget{pds: pds, did: did})
	}
	return c.reconcileObject(ctx, ri, nil, "", u, object, repoTarget{pds: pds, did: did}, false)
}

func decodeRecord(u *unstructured.Unstructured) (v1alpha1.AtprotoRecord, error) {
	var object v1alpha1.AtprotoRecord
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &object); err != nil {
		return object, fmt.Errorf("recordcontroller: decoding AtprotoRecord %s: %w", u.GetName(), err)
	}
	return object, nil
}

func (c *Controller) reconcileObject(ctx context.Context, ri dynamic.ResourceInterface, dyn dynamic.Interface, workspace string,
	u *unstructured.Unstructured, object v1alpha1.AtprotoRecord, target repoTarget, addFinalizer bool) error {
	if target.pds == nil {
		return c.writeFailure(ctx, ri, u, object, "RepoRef", errors.New("the record has no repo"))
	}
	if addFinalizer && !slices.Contains(object.Finalizers, v1alpha1.FinalizerAtprotoRecord) {
		object.Finalizers = append(object.Finalizers, v1alpha1.FinalizerAtprotoRecord)
		if err := c.patchFinalizers(ctx, ri, u.GetName(), object.Finalizers); err != nil {
			return err
		}
	}
	path, err := recordPath(target.did, object.Spec)
	if err != nil {
		return c.writeFailure(ctx, ri, u, object, "InvalidSpec", err)
	}
	resolved, missing, err := resolveReferences(ctx, dyn, c.dynamic, workspace, object.Spec.Record)
	if err != nil {
		return c.writeFailure(ctx, ri, u, object, "InvalidSpec", err)
	}
	if len(missing) > 0 {
		message := "the record references: " + strings.Join(missing, "; ")
		if object.Spec.OnMissingReference == v1alpha1.MissingReferenceDelete {
			if err := c.deleteRecord(ctx, target, path); err != nil {
				return c.writeFailure(ctx, ri, u, object, "DeleteFailed", err)
			}
			return c.writeStatus(ctx, ri, u, object, v1alpha1.AtprotoRecordPhasePending, target.did.String(), "", "", message,
				conds.New(ConditionReferencesResolved, false, "MissingReference", message, object.Generation),
				conds.New(ConditionInSync, true, "Deleted", "the record is absent because a reference is missing", object.Generation))
		}
		return c.writeStatus(ctx, ri, u, object, v1alpha1.AtprotoRecordPhasePending, target.did.String(), "", "", message,
			conds.New(ConditionReferencesResolved, false, "MissingReference", message, object.Generation),
			conds.New(ConditionInSync, false, "MissingReference", "a reference is missing", object.Generation))
	}
	observed, absent, err := getRecord(ctx, target.pds, path)
	if err != nil {
		return c.writeFailure(ctx, ri, u, object, "ObserveFailed", err)
	}
	reason := "InSync"
	message := ""
	uri := ""
	cid := ""
	write := false
	switch {
	case absent:
		reason = "Created"
		write = true
	case !recordsEqual(observed.Value, resolved):
		reason = "Repaired"
		message = "the repo held cid " + observed.CID.String() + "; the declared record was written"
		write = true
	default:
		uri = observed.URI.String()
		cid = observed.CID.String()
	}
	if write {
		out, err := target.pds.PutRecord(ctx, path, resolved, pdsclient.WriteOptions{})
		if err != nil {
			return c.writeFailure(ctx, ri, u, object, "WriteFailed", err)
		}
		uri, cid = out.URI.String(), out.CID.String()
		c.log.Info("an AtprotoRecord wrote its declared record", "name", object.Name, "uri", uri, "cid", cid, "reason", reason)
	}
	return c.writeStatus(ctx, ri, u, object, v1alpha1.AtprotoRecordPhaseApplied, target.did.String(), uri, cid, message,
		conds.New(ConditionReferencesResolved, true, "Resolved", "every reference is resolved", object.Generation),
		conds.New(ConditionInSync, true, reason, "the repo holds the declared record", object.Generation))
}

func (c *Controller) reconcileDelete(ctx context.Context, ri dynamic.ResourceInterface, dyn dynamic.Interface, workspace string,
	u *unstructured.Unstructured, target *repoTarget) error {
	object, err := decodeRecord(u)
	if err != nil {
		return err
	}
	if !slices.Contains(object.Finalizers, v1alpha1.FinalizerAtprotoRecord) {
		return nil
	}
	if target == nil {
		resolved, err := c.resolveRepoTarget(ctx, dyn, workspace, object)
		if err != nil {
			return err
		}
		target = &resolved
	}
	path, err := recordPath(target.did, object.Spec)
	if err != nil {
		return err
	}
	if err := c.deleteRecord(ctx, *target, path); err != nil {
		return err
	}
	finalizers := slices.DeleteFunc(slices.Clone(object.Finalizers), func(f string) bool {
		return f == v1alpha1.FinalizerAtprotoRecord
	})
	return c.patchFinalizers(ctx, ri, u.GetName(), finalizers)
}

func (c *Controller) deleteRecord(ctx context.Context, target repoTarget, path pdsclient.RepoPath) error {
	err := target.pds.DeleteRecord(ctx, path, pdsclient.WriteOptions{})
	if err == nil {
		return nil
	}
	var xe *pdsclient.XRPCError
	if errors.As(err, &xe) && (xe.Code == "RecordNotFound" || xe.Code == "RepoNotFound" || xe.StatusCode == http.StatusNotFound) {
		return nil
	}
	return err
}

func recordPath(did syntax.DID, spec v1alpha1.AtprotoRecordSpec) (pdsclient.RepoPath, error) {
	collection := strings.TrimSpace(spec.Collection)
	if collection == "" {
		return pdsclient.RepoPath{}, errors.New("the record names no collection")
	}
	rkey := strings.TrimSpace(spec.Rkey)
	if rkey == "" {
		return pdsclient.RepoPath{}, errors.New("the record names no rkey")
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return pdsclient.RepoPath{}, fmt.Errorf("the collection %q: %w", collection, err)
	}
	key, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return pdsclient.RepoPath{}, fmt.Errorf("the rkey %q: %w", rkey, err)
	}
	return pdsclient.RepoPath{Repo: did, Collection: nsid, Rkey: key}, nil
}

func getRecord(ctx context.Context, pds pdsclient.PDS, path pdsclient.RepoPath) (pdsclient.Record, bool, error) {
	record, err := pds.GetRecord(ctx, path, nil)
	if err == nil {
		return record, false, nil
	}
	var xe *pdsclient.XRPCError
	if errors.As(err, &xe) && (xe.Code == "RecordNotFound" || xe.Code == "RepoNotFound" || xe.StatusCode == http.StatusNotFound) {
		return pdsclient.Record{}, true, nil
	}
	return pdsclient.Record{}, false, err
}

func recordsEqual(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func (c *Controller) resolveRepoTarget(ctx context.Context, dyn dynamic.Interface, workspace string, object v1alpha1.AtprotoRecord) (repoTarget, error) {
	if c.resolver == nil {
		return repoTarget{}, errors.New("no repo resolver is configured")
	}
	binding, err := c.repoBinding(ctx, dyn, workspace, object.Spec)
	if err != nil {
		return repoTarget{}, err
	}
	resolved, err := c.resolver.Resolve(ctx, binding)
	if err != nil {
		return repoTarget{}, err
	}
	if resolved.DID != "" {
		return repoTarget{pds: resolved.PDS, did: resolved.DID}, nil
	}
	info, err := resolved.PDS.Describe(ctx)
	if err != nil {
		return repoTarget{}, fmt.Errorf("reading the repo did: %w", err)
	}
	return repoTarget{pds: resolved.PDS, did: info.DID}, nil
}

func (c *Controller) repoBinding(ctx context.Context, dyn dynamic.Interface, workspace string, spec v1alpha1.AtprotoRecordSpec) (pdsclient.RepoBinding, error) {
	if spec.RepoRef != nil {
		return c.repoBindingForRef(ctx, dyn, workspace, *spec.RepoRef)
	}
	repo := strings.TrimSpace(spec.Repo)
	if repo == "" {
		return pdsclient.RepoBinding{}, errors.New("the record names no repoRef")
	}
	if strings.HasPrefix(repo, "did:") {
		return pdsclient.RepoBinding{}, errors.New("the legacy repo field names a DID; declare a repoRef instead")
	}
	if strings.Contains(repo, "://") {
		return pdsclient.RepoBinding{Workspace: workspace, Kind: "url", Endpoint: repo}, nil
	}
	return pdsclient.RepoBinding{Workspace: workspace, Kind: "Pds", Name: repo}, nil
}

func (c *Controller) repoBindingForRef(ctx context.Context, dyn dynamic.Interface, workspace string, ref commonv1.ObjectRef) (pdsclient.RepoBinding, error) {
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return pdsclient.RepoBinding{}, errors.New("the repoRef names no object")
	}
	refWorkspace := strings.TrimSpace(ref.Workspace)
	if refWorkspace == "" {
		refWorkspace = workspace
	}
	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "atprotorepo":
		client, err := c.clientFor(dyn, workspace, refWorkspace)
		if err != nil {
			return pdsclient.RepoBinding{}, err
		}
		object, err := client.Resource(AtprotoRepoGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return pdsclient.RepoBinding{}, fmt.Errorf("reading AtprotoRepo %s: %w", name, err)
		}
		var repo v1alpha1.AtprotoRepo
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &repo); err != nil {
			return pdsclient.RepoBinding{}, fmt.Errorf("decoding AtprotoRepo %s: %w", name, err)
		}
		backend, err := pdsclient.ReadRepoBackend(ctx, client, refWorkspace, repo)
		if err != nil {
			return pdsclient.RepoBinding{}, err
		}
		binding := backend.Binding()
		if strings.TrimSpace(binding.DID) == "" {
			return pdsclient.RepoBinding{}, fmt.Errorf("AtprotoRepo %s names no did", name)
		}
		if binding.Name == "" {
			return pdsclient.RepoBinding{}, fmt.Errorf("AtprotoRepo %s names no pdsRef", name)
		}
		return binding, nil
	case "remotepds", "remote", "remote-pds":
		client, err := c.clientFor(dyn, workspace, refWorkspace)
		if err != nil {
			return pdsclient.RepoBinding{}, err
		}
		object, err := client.Resource(RemotePDSGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return pdsclient.RepoBinding{}, fmt.Errorf("reading RemotePDS %s: %w", name, err)
		}
		var remote pdsv1alpha1.RemotePDS
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &remote); err != nil {
			return pdsclient.RepoBinding{}, fmt.Errorf("decoding RemotePDS %s: %w", name, err)
		}
		return pdsclient.RepoBinding{Workspace: refWorkspace, Kind: "RemotePDS", Name: name, DID: strings.TrimSpace(remote.Spec.DID)}, nil
	case "", "pds", "local", "localpds", "local-pds":
		kind := strings.TrimSpace(ref.Kind)
		if kind == "" {
			kind = "Pds"
		}
		if repo, err := localRepoBinding(ctx, dyn, workspace, kind, name); err != nil {
			return pdsclient.RepoBinding{}, err
		} else if repo != nil {
			return *repo, nil
		}
		return pdsclient.RepoBinding{Workspace: refWorkspace, Kind: "Pds", Name: name}, nil
	default:
		return pdsclient.RepoBinding{Workspace: refWorkspace, Kind: ref.Kind, Name: name}, nil
	}
}

func localRepoBinding(ctx context.Context, dyn dynamic.Interface, workspace, kind, name string) (*pdsclient.RepoBinding, error) {
	if dyn == nil {
		return nil, nil
	}
	if strings.TrimSpace(kind) == "" || strings.EqualFold(kind, "pds") {
		kind = "Pds"
	}
	backends, err := pdsclient.ListRepoBackends(ctx, dyn, workspace)
	if err != nil {
		return nil, err
	}
	for _, backend := range backends {
		if backend.PdsRef.Name != name || !strings.EqualFold(strings.TrimSpace(backend.PdsRef.Kind), kind) || backend.DID == "" {
			continue
		}
		binding := &pdsclient.RepoBinding{Workspace: workspace, Kind: kind, Name: name, DID: backend.DID}
		if backend.AuthRef != nil {
			binding.AuthRef = backend.AuthRef.DeepCopy()
		}
		return binding, nil
	}
	return nil, nil
}

func (c *Controller) clientFor(dyn dynamic.Interface, workspace, target string) (dynamic.Interface, error) {
	if target == workspace || target == "" {
		if dyn == nil {
			return nil, errors.New("no dynamic client")
		}
		return dyn, nil
	}
	if c.dynamic == nil {
		return nil, errors.New("no dynamic factory for another workspace")
	}
	return c.dynamic(target)
}

func (c *Controller) writeFailure(ctx context.Context, ri dynamic.ResourceInterface, u *unstructured.Unstructured,
	object v1alpha1.AtprotoRecord, reason string, cause error) error {
	message := cause.Error()
	c.log.Warn("an AtprotoRecord failed", "name", object.Name, "reason", reason, "err", message)
	return c.writeStatus(ctx, ri, u, object, v1alpha1.AtprotoRecordPhaseFailed, object.Status.Did, object.Status.URI, object.Status.Cid,
		message,
		conds.New(ConditionReferencesResolved, false, reason, message, object.Generation),
		conds.New(ConditionInSync, false, reason, message, object.Generation))
}

func (c *Controller) writeStatus(ctx context.Context, ri dynamic.ResourceInterface, u *unstructured.Unstructured,
	object v1alpha1.AtprotoRecord, phase v1alpha1.AtprotoRecordPhase, did string, uri, cid, message string,
	conditions ...metav1.Condition) error {
	status := object.Status
	status.Phase = phase
	status.ObservedGeneration = object.Generation
	status.Did = did
	status.URI = uri
	status.Cid = cid
	status.Message = message
	status.Conditions = append([]metav1.Condition(nil), object.Status.Conditions...)
	for _, want := range conditions {
		meta.SetStatusCondition(&status.Conditions, want)
	}
	if !statusChanged(object.Status, status) {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, u.GetName(), types.MergePatchType, raw, metav1.PatchOptions{}, "status")
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("recordcontroller: writing the status of %s: %w", u.GetName(), err)
	}
	return nil
}

func statusChanged(before, after v1alpha1.AtprotoRecordStatus) bool {
	normalize := func(status v1alpha1.AtprotoRecordStatus) v1alpha1.AtprotoRecordStatus {
		status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
		slices.SortFunc(status.Conditions, func(a, b metav1.Condition) int {
			return strings.Compare(a.Type, b.Type)
		})
		for i := range status.Conditions {
			status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		return status
	}
	return !reflect.DeepEqual(normalize(before), normalize(after))
}

func (c *Controller) patchFinalizers(ctx context.Context, ri dynamic.ResourceInterface, name string, finalizers []string) error {
	if finalizers == nil {
		finalizers = []string{}
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": finalizers}})
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

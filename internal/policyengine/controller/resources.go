package controller

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/statuspatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

type Resource[T any] struct {
	store *kcpstore.Store

	gvr schema.GroupVersionResource

	kind string
}

func NewResource[T any](store *kcpstore.Store, gvr schema.GroupVersionResource, kind string) *Resource[T] {
	return &Resource[T]{store: store, gvr: gvr, kind: kind}
}

func (r *Resource[T]) client(cluster string) (rest.Interface, error) {
	return r.store.ClientFor(cluster, r.gvr.GroupVersion())
}

func (r *Resource[T]) Get(ctx context.Context, cluster, name string) (*T, error) {
	client, err := r.client(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := client.Get().Resource(r.gvr.Resource).Name(name).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("policy resource: read %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	return decodeObject[T](raw)
}

func (r *Resource[T]) List(ctx context.Context, cluster string) ([]T, error) {
	client, err := r.client(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := client.Get().Resource(r.gvr.Resource).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("policy resource: list %s in %s: %w", r.gvr.Resource, cluster, err)
	}
	var list struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("policy resource: decode %s list: %w", r.gvr.Resource, err)
	}
	return list.Items, nil
}

func (r *Resource[T]) Create(ctx context.Context, cluster string, object *T) error {
	client, err := r.client(cluster)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("policy resource: encode %s: %w", r.gvr.Resource, err)
	}
	envelope := map[string]any{}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if _, ok := envelope["apiVersion"]; !ok {
		envelope["apiVersion"] = r.gvr.GroupVersion().String()
	}
	if _, ok := envelope["kind"]; !ok {
		envelope["kind"] = r.kind
	}
	raw, err = json.Marshal(envelope)
	if err != nil {
		return err
	}
	response, err := client.Post().Resource(r.gvr.Resource).Body(raw).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("policy resource: create %s in %s: %w%s", r.gvr.Resource, cluster, err, kcpstore.ResponseDetail(response))
	}
	return nil
}

func (r *Resource[T]) Delete(ctx context.Context, cluster, name string) error {
	client, err := r.client(cluster)
	if err != nil {
		return err
	}
	err = client.Delete().Resource(r.gvr.Resource).Name(name).Body([]byte(`{"propagationPolicy":"Background"}`)).Do(ctx).Error()
	if err != nil && !kcpstore.IsNotFound(err) {
		return fmt.Errorf("policy resource: delete %s %s in %s: %w", r.gvr.Resource, name, cluster, err)
	}
	return nil
}

func (r *Resource[T]) PatchStatus(ctx context.Context, cluster, name, resourceVersion string, patch []byte) error {
	client, err := r.client(cluster)
	if err != nil {
		return err
	}
	body, err := statuspatch.WithResourceVersion(patch, resourceVersion)
	if err != nil {
		return err
	}
	response, err := client.Patch(types.MergePatchType).SubResource("status").Resource(r.gvr.Resource).Name(name).Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("policy resource: write %s status for %s in %s: %w%s", r.gvr.Resource, name, cluster, err, kcpstore.ResponseDetail(response))
	}
	return nil
}

func (r *Resource[T]) Patch(ctx context.Context, cluster, name string, patch []byte) error {
	client, err := r.client(cluster)
	if err != nil {
		return err
	}
	response, err := client.Patch(types.MergePatchType).Resource(r.gvr.Resource).Name(name).Body(patch).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("policy resource: patch %s %s in %s: %w%s", r.gvr.Resource, name, cluster, err, kcpstore.ResponseDetail(response))
	}
	return nil
}

func (r *Resource[T]) PatchJSON(ctx context.Context, cluster, name string, patch []byte) error {
	client, err := r.client(cluster)
	if err != nil {
		return err
	}
	response, err := client.Patch(types.JSONPatchType).Resource(r.gvr.Resource).Name(name).Body(patch).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("policy resource: patch %s %s in %s: %w%s", r.gvr.Resource, name, cluster, err, kcpstore.ResponseDetail(response))
	}
	return nil
}

func decodeObject[T any](raw []byte) (*T, error) {
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("policy resource: decode object: %w", err)
	}
	return &out, nil
}

type Resources struct {
	Cluster string

	Engines *Resource[policyv1alpha1.PolicyEngine]

	Workflows *Resource[policyv1alpha1.PolicyWorkflow]

	Runs *Resource[policyv1alpha1.PolicyWorkflowRun]

	Triggers *Resource[policyv1alpha1.PolicyTrigger]
}

func NewResources(store *kcpstore.Store, cluster string) *Resources {
	return &Resources{
		Cluster:   cluster,
		Engines:   NewResource[policyv1alpha1.PolicyEngine](store, policyv1alpha1.PolicyEngineGVR, "PolicyEngine"),
		Workflows: NewResource[policyv1alpha1.PolicyWorkflow](store, policyv1alpha1.PolicyWorkflowGVR, "PolicyWorkflow"),
		Runs:      NewResource[policyv1alpha1.PolicyWorkflowRun](store, policyv1alpha1.PolicyWorkflowRunGVR, "PolicyWorkflowRun"),
		Triggers:  NewResource[policyv1alpha1.PolicyTrigger](store, policyv1alpha1.PolicyTriggerGVR, "PolicyTrigger"),
	}
}

func (r *Resources) GetWorkflow(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflow, error) {
	return r.Workflows.Get(ctx, r.Cluster, name)
}

func (r *Resources) GetRun(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflowRun, error) {
	return r.Runs.Get(ctx, r.Cluster, name)
}

func (r *Resources) CreateRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) error {
	return r.Runs.Create(ctx, r.Cluster, run)
}

type WorkflowSourceFunc func(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflow, error)

func (f WorkflowSourceFunc) GetPolicyWorkflow(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflow, error) {
	return f(ctx, name)
}

func (r *Resources) WorkflowSource() WorkflowSource {
	return WorkflowSourceFunc(r.GetWorkflow)
}

func nilIfEmpty[T any](value []T) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nilIfEmptyMap[K comparable, V any](value map[K]V) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func (r *Resources) WriteEngineStatus(ctx context.Context, engine *policyv1alpha1.PolicyEngine, state EngineReconcileState) error {
	status := map[string]any{
		"phase":      string(state.Phase),
		"ready":      state.Ready,
		"activeRuns": state.ActiveRuns,
		"conditions": nilIfEmpty(state.Conditions),
	}
	return writeStatus(ctx, r.Engines, r.Cluster, engine.Name, func(value *policyv1alpha1.PolicyEngine) string { return value.ResourceVersion }, status)
}

func (r *Resources) WriteWorkflowStatus(ctx context.Context, workflow *policyv1alpha1.PolicyWorkflow, state WorkflowReconcileState) error {
	status := map[string]any{
		"phase":              string(state.Phase),
		"observedGeneration": workflow.Generation,
		"conditions":         nilIfEmpty(state.Conditions),
	}
	return writeStatus(ctx, r.Workflows, r.Cluster, workflow.Name, func(value *policyv1alpha1.PolicyWorkflow) string { return value.ResourceVersion }, status)
}

func (r *Resources) WriteRunStatus(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, value policyv1alpha1.PolicyWorkflowRunStatus) error {
	status := map[string]any{
		"phase":          string(value.Phase),
		"runID":          value.RunID,
		"outputs":        nilIfEmptyMap(value.Outputs),
		"cache":          nilIfEmptyMap(value.Cache),
		"consoleOutput":  value.ConsoleOutput,
		"annotations":    nilIfEmptyMap(value.Annotations),
		"restarts":       value.Restarts,
		"startTime":      value.StartTime,
		"completionTime": value.CompletionTime,
		"conditions":     nilIfEmpty(value.Conditions),
	}
	if value.ExitStatus != "" {
		status["exitStatus"] = string(value.ExitStatus)
	}
	return writeStatus(ctx, r.Runs, r.Cluster, run.Name, func(value *policyv1alpha1.PolicyWorkflowRun) string { return value.ResourceVersion }, status)
}

func (r *Resources) WriteTriggerStatus(ctx context.Context, trigger *policyv1alpha1.PolicyTrigger, state TriggerReconcileState) error {
	status := map[string]any{
		"lastRun":    state.LastRun,
		"nextRun":    state.NextRun,
		"active":     state.Active,
		"conditions": nilIfEmpty(state.Conditions),
	}
	return writeStatus(ctx, r.Triggers, r.Cluster, trigger.Name, func(value *policyv1alpha1.PolicyTrigger) string { return value.ResourceVersion }, status)
}

func (r *Resources) DeleteRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) error {
	return r.Runs.Delete(ctx, r.Cluster, run.Name)
}

func writeStatus[T any](ctx context.Context, resource *Resource[T], cluster, name string, resourceVersion func(*T) string, status map[string]any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	var lastErr error
	for range 3 {
		current, getErr := resource.Get(ctx, cluster, name)
		if getErr != nil {
			if kcpstore.IsNotFound(getErr) {
				return nil
			}
			return getErr
		}
		lastErr = resource.PatchStatus(ctx, cluster, name, resourceVersion(current), body)
		if lastErr == nil {
			return nil
		}
		if !apierrors.IsConflict(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

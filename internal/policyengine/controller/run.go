package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	perspectiveBidder    = "bidder"
	perspectiveRequester = "requester"
)

type RunReconciler struct {
	Engine    *Engine
	Workflows WorkflowSource
	Records   RecordSource
	Host      trust.Host
	Now       func() time.Time
}

func (r *RunReconciler) ReconcilePolicyWorkflowRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) (Outcome, error) {
	if run == nil {
		return Outcome{}, fmt.Errorf("policy workflow run is required")
	}
	if run.Spec.Cancel || run.DeletionTimestamp != nil {
		return cancelledOutcome(run, policyengine.ConditionCancelled, "Cancelled", "the run was cancelled"), nil
	}
	if run.Spec.Suspend {
		return cancelledOutcome(run, policyengine.ConditionSuspended, "Suspended", "the run is suspended"), nil
	}
	if r.Engine == nil {
		return failedOutcome(fmt.Errorf("policy engine is not configured")), nil
	}
	workflowRaw, workflowCR, err := r.resolveWorkflow(ctx, run)
	if err != nil {
		return failedOutcome(err), nil
	}
	seed := runCacheSeed(run)
	inputs, err := r.buildInputs(ctx, run, workflowCR, seed)
	if err != nil {
		return failedOutcome(err), nil
	}
	requestContext, contextErr := r.requestContext(ctx, run, workflowCR, inputs)
	if contextErr != nil {
		if ctx.Err() != nil {
			return failedOutcome(ctx.Err()), nil
		}
		return failedOutcome(contextErr), nil
	}
	request := &PolicyEngineRequest{
		Inputs:   inputs,
		Workflow: workflowRaw,
		Context:  requestContext,
	}
	opts := RunOptions{
		Deadline: r.deadline(run, workflowCR),
		Cache:    seed,
	}
	outcome, err := r.Engine.Execute(ctx, request, opts)
	if err != nil {
		return failedOutcome(err), nil
	}
	outcome.Status.Restarts = run.Status.Restarts
	return outcome, nil
}

func (r *RunReconciler) resolveWorkflow(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) (json.RawMessage, *policyv1alpha1.PolicyWorkflow, error) {
	if len(bytes.TrimSpace(run.Spec.Workflow)) > 0 && !bytes.Equal(bytes.TrimSpace(run.Spec.Workflow), []byte("null")) {
		raw, err := normalizeWorkflow(run.Spec.Workflow)
		if err != nil {
			return nil, nil, err
		}
		return raw, nil, nil
	}
	if run.Spec.WorkflowRef == "" {
		return nil, nil, fmt.Errorf("workflowRef or workflow is required")
	}
	seed := runCacheSeed(run)
	if r.Workflows != nil {
		workflowCR, getErr := r.Workflows.GetPolicyWorkflow(ctx, run.Spec.WorkflowRef)
		if getErr == nil && workflowCR != nil {
			if workflowCR.Spec.RecordRef != nil {
				if r.Records == nil {
					return nil, workflowCR, fmt.Errorf("policy workflow %s requires a record resolver", run.Spec.WorkflowRef)
				}
				raw, resolveErr := resolveWorkflowRef(ctx, r.Records, *workflowCR.Spec.RecordRef, seed)
				if resolveErr != nil {
					return nil, workflowCR, fmt.Errorf("resolve workflow record for %s: %w", run.Spec.WorkflowRef, resolveErr)
				}
				normalized, normalizeErr := normalizeWorkflow(raw)
				if normalizeErr != nil {
					return nil, workflowCR, normalizeErr
				}
				return normalized, workflowCR, nil
			}
			if len(bytes.TrimSpace(workflowCR.Spec.Workflow)) > 0 {
				raw, normalizeErr := normalizeWorkflow(workflowCR.Spec.Workflow)
				if normalizeErr != nil {
					return nil, workflowCR, normalizeErr
				}
				return raw, workflowCR, nil
			}
			if raw, ok := bundledWorkflow(workflowCR.Name); ok {
				return raw, workflowCR, nil
			}
		}
		if getErr != nil && !kcpstore.IsNotFound(getErr) {
			if _, ok := bundledWorkflow(run.Spec.WorkflowRef); !ok {
				return nil, nil, fmt.Errorf("read policy workflow %s: %w", run.Spec.WorkflowRef, getErr)
			}
		}
	}
	if raw, ok := bundledWorkflow(run.Spec.WorkflowRef); ok {
		return raw, nil, nil
	}
	return nil, nil, fmt.Errorf("policy workflow %s not found", run.Spec.WorkflowRef)
}

func (r *RunReconciler) buildInputs(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, workflowCR *policyv1alpha1.PolicyWorkflow, seed models.Cache) (map[string]any, error) {
	inputs := map[string]any{}
	defaultArgs := map[string]string{}
	selfDid := run.Spec.SelfDid
	perspective := run.Spec.Perspective
	if workflowCR != nil {
		for key, value := range workflowCR.Spec.DefaultInputs {
			inputs[key] = value
		}
		maps.Copy(defaultArgs, workflowCR.Spec.DefaultArgs)
		if selfDid == "" {
			selfDid = workflowCR.Spec.DefaultSelfDid
		}
		if perspective == "" {
			perspective = workflowCR.Spec.DefaultPerspective
		}
	}
	args := make(map[string]string, len(defaultArgs)+len(run.Spec.Args))
	maps.Copy(args, defaultArgs)
	maps.Copy(args, run.Spec.Args)
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	inputs["policy-args"] = string(argsJSON)
	subjectDid := run.Spec.SubjectDid
	counterpartyDid := run.Spec.CounterpartyDid
	rootRequesterDid := run.Spec.RootRequesterDid
	if counterpartyDid == "" {
		switch perspective {
		case perspectiveRequester:
			if run.Spec.Bid != nil {
				counterpartyDid = atproto.RepoFromURI(run.Spec.Bid.URI)
			}
		case perspectiveBidder:
			if run.Spec.RFP != nil {
				counterpartyDid = atproto.RepoFromURI(run.Spec.RFP.URI)
			} else if run.Spec.Accept != nil {
				counterpartyDid = atproto.RepoFromURI(run.Spec.Accept.URI)
			}
		}
	}
	if subjectDid == "" {
		subjectDid = counterpartyDid
	}
	if rootRequesterDid == "" {
		if perspective == perspectiveRequester {
			rootRequesterDid = selfDid
		} else {
			rootRequesterDid = counterpartyDid
		}
	}
	if selfDid != "" {
		inputs["self-did"] = selfDid
	}
	if perspective != "" {
		inputs["perspective"] = perspective
	}
	if subjectDid != "" {
		inputs["subject-did"] = subjectDid
	}
	if counterpartyDid != "" {
		inputs["counterparty-did"] = counterpartyDid
	}
	if rootRequesterDid != "" {
		inputs["root-requester-did"] = rootRequesterDid
	}
	if value := r.recordInput(ctx, run.Spec.RFP, seed); value != "" {
		inputs["rfp"] = value
	}
	if value := r.recordInput(ctx, run.Spec.Bid, seed); value != "" {
		inputs["bid"] = value
	}
	if value := r.recordInput(ctx, run.Spec.Accept, seed); value != "" {
		inputs["accept"] = value
	}
	for key, value := range run.Spec.Inputs {
		inputs[key] = value
	}
	return inputs, nil
}

func (r *RunReconciler) recordInput(ctx context.Context, ref *commonv1.StrongRef, seed models.Cache) string {
	if ref == nil || ref.URI == "" {
		return ""
	}
	entry := map[string]any{"uri": ref.URI, "cid": ref.CID}
	if r.Records != nil {
		if value, err := resolveRecordRef(ctx, r.Records, *ref, seed); err == nil && value != nil {
			entry["value"] = value
		}
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(data)
}

func resolveRecordRef(ctx context.Context, source RecordSource, ref commonv1.StrongRef, seed models.Cache) (map[string]any, error) {
	if cached, ok := source.(RecordCacheSource); ok {
		return cached.ResolveRecordCached(ctx, ref, seed)
	}
	return source.ResolveRecord(ctx, ref)
}

func resolveWorkflowRef(ctx context.Context, source RecordSource, ref commonv1.StrongRef, seed models.Cache) (json.RawMessage, error) {
	if cached, ok := source.(RecordCacheSource); ok {
		return cached.ResolveWorkflowCached(ctx, ref, seed)
	}
	return source.ResolveWorkflow(ctx, ref)
}

func runCacheSeed(run *policyv1alpha1.PolicyWorkflowRun) models.Cache {
	if run == nil {
		return models.Cache{}
	}
	if len(run.Spec.Cache) > 0 {
		return CacheFromAPI(run.Spec.Cache)
	}
	if len(run.Status.Cache) > 0 {
		return CacheFromAPI(run.Status.Cache)
	}
	return models.Cache{}
}

func (r *RunReconciler) requestContext(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, workflowCR *policyv1alpha1.PolicyWorkflow, inputs map[string]any) (map[string]any, error) {
	selfDid := stringInput(inputs, "self-did")
	counterpartyDid := stringInput(inputs, "counterparty-did")
	env := map[string]any{}
	if selfDid != "" {
		env["GITHUB_ACTOR"] = selfDid
	}
	repository := ""
	if workflowCR != nil && workflowCR.Spec.RecordRef != nil {
		repository = atproto.RepoFromURI(workflowCR.Spec.RecordRef.URI)
	}
	if repository == "" {
		repository = selfDid
	}
	if repository != "" {
		env["GITHUB_REPOSITORY"] = repository
	}
	context := map[string]any{"config": map[string]any{"env": env}}
	cache := runCacheSeed(run)
	if cache == nil {
		cache = models.Cache{}
	}
	if r.Host != nil && selfDid != "" && counterpartyDid != "" {
		if _, seeded := cache["trust/"+counterpartyDid]; !seeded {
			selfService, counterpartyService := trust.BidderAssociate, trust.RequesterAssociate
			if stringInput(inputs, "perspective") == perspectiveRequester {
				selfService, counterpartyService = trust.RequesterAssociate, trust.BidderAssociate
			}
			snapshot, err := trust.BuildSnapshotFor(ctx, r.Host, selfDid, counterpartyDid, selfService, counterpartyService)
			if err != nil {
				return nil, err
			}
			cache["trust/"+counterpartyDid] = snapshot.CacheFiles()
		}
	}
	if len(cache) > 0 {
		context["cache"] = cache
	}
	return context, nil
}

func stringInput(inputs map[string]any, key string) string {
	value, ok := inputs[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func (r *RunReconciler) deadline(run *policyv1alpha1.PolicyWorkflowRun, workflowCR *policyv1alpha1.PolicyWorkflow) time.Duration {
	var deadline time.Duration
	if run.Spec.ActiveDeadlineSeconds != nil && *run.Spec.ActiveDeadlineSeconds > 0 {
		deadline = time.Duration(*run.Spec.ActiveDeadlineSeconds) * time.Second
	}
	if workflowCR != nil && workflowCR.Spec.TimeoutSeconds != nil && *workflowCR.Spec.TimeoutSeconds > 0 {
		value := time.Duration(*workflowCR.Spec.TimeoutSeconds) * time.Second
		if deadline <= 0 || value < deadline {
			deadline = value
		}
	}
	return deadline
}

func normalizeWorkflow(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("workflow is required")
	}
	if json.Valid(trimmed) {
		var text string
		if json.Unmarshal(trimmed, &text) == nil && text == "" {
			return nil, fmt.Errorf("workflow is required")
		}
		return append(json.RawMessage(nil), trimmed...), nil
	}
	data, err := json.Marshal(string(trimmed))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func bundledWorkflow(name string) (json.RawMessage, bool) {
	data, ok := bundledactions.Workflow(name)
	if !ok {
		return nil, false
	}
	encoded, err := json.Marshal(string(data))
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func cancelledOutcome(run *policyv1alpha1.PolicyWorkflowRun, conditionType, reason, message string) Outcome {
	completed := metav1.Now()
	statusValue := policyengine.Status{
		Phase:       policyengine.PhaseCancelled,
		RunID:       run.Status.RunID,
		CompletedAt: &completed,
		Restarts:    run.Status.Restarts,
	}
	if run.Status.StartTime != nil {
		statusValue.StartedAt = run.Status.StartTime
	}
	meta.SetStatusCondition(&statusValue.Conditions, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	return Outcome{
		Result: models.Result{
			RunID:      statusValue.RunID,
			Status:     models.ExecutionComplete,
			ExitStatus: models.ExitFailure,
			Cache:      CacheFromAPI(run.Status.Cache),
		},
		Status: statusValue,
	}
}

func failedOutcome(err error) Outcome {
	completed := metav1.Now()
	message := "the policy run failed"
	if err != nil {
		message = err.Error()
	}
	statusValue := policyengine.Status{
		Phase:       policyengine.PhaseFailed,
		ExitStatus:  models.ExitFailure,
		CompletedAt: &completed,
		Annotations: models.Annotations{
			"error": []models.Annotation{{
				AnnotationLevel: models.AnnotationError,
				Message:         message,
				Title:           message,
			}},
		},
	}
	meta.SetStatusCondition(&statusValue.Conditions, metav1.Condition{
		Type:    policyengine.ConditionFailed,
		Status:  metav1.ConditionTrue,
		Reason:  "Error",
		Message: message,
	})
	return Outcome{
		Result: models.Result{
			Status:      models.ExecutionComplete,
			ExitStatus:  models.ExitFailure,
			Annotations: statusValue.Annotations,
		},
		Status: statusValue,
	}
}

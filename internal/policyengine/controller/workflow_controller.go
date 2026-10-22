package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/workflow"
)

type WorkflowReconciler struct {
	Actions workflow.ActionResolver

	Records RecordSource
}

func (r *WorkflowReconciler) ReconcilePolicyWorkflow(ctx context.Context, workflowCR *policyv1alpha1.PolicyWorkflow) (WorkflowReconcileState, error) {
	if workflowCR == nil {
		return WorkflowReconcileState{}, fmt.Errorf("policy workflow is required")
	}
	if workflowCR.DeletionTimestamp != nil {
		state := WorkflowReconcileState{Phase: policyv1alpha1.PolicyWorkflowPending}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    policyengine.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Terminating",
			Message: "the policy workflow is being deleted",
		})
		return state, nil
	}
	var raw json.RawMessage
	if workflowCR.Spec.RecordRef != nil {
		if r.Records == nil {
			return workflowInvalid("the policy workflow record resolver is not configured"), nil
		}
		resolved, err := r.Records.ResolveWorkflow(ctx, *workflowCR.Spec.RecordRef)
		if err != nil {
			if ctx.Err() != nil {
				return WorkflowReconcileState{Phase: policyv1alpha1.PolicyWorkflowPending}, ctx.Err()
			}
			return workflowInvalid(err.Error()), nil
		}
		raw = resolved
	} else {
		if len(bytes.TrimSpace(workflowCR.Spec.Workflow)) == 0 {
			if bundled, ok := bundledWorkflow(workflowCR.Name); ok {
				return r.validate(bundled), nil
			}
			return workflowInvalid("workflow or recordRef is required"), nil
		}
		raw = workflowCR.Spec.Workflow
	}
	return r.validate(raw), nil
}

func (r *WorkflowReconciler) validate(raw json.RawMessage) WorkflowReconcileState {
	parsed, err := workflow.Parse(raw)
	if err != nil {
		return workflowInvalid(err.Error())
	}
	if job, ok := workflow.HasRunStep(raw); ok {
		return workflowInvalid(fmt.Sprintf("%s a run: step (job %s)", executor.NetOnlyRefused, job))
	}
	resolver := r.Actions
	if resolver == nil {
		resolver = executor.DefaultActionResolver()
	}
	for _, job := range parsed.Jobs {
		for _, step := range job.Steps {
			if step.Uses == "" {
				continue
			}
			action, actionErr := resolver(step.Uses)
			if actionErr != nil {
				return workflowInvalid(actionErr.Error())
			}
			if !strings.HasPrefix(action.Using, "node") {
				return workflowInvalid(fmt.Sprintf("action %s uses unsupported runtime %s", step.Uses, action.Using))
			}
		}
	}
	return workflowReady("Valid", "the gha-lite workflow is valid")
}

func workflowReady(reason, message string) WorkflowReconcileState {
	state := WorkflowReconcileState{Phase: policyv1alpha1.PolicyWorkflowReady}
	meta.SetStatusCondition(&state.Conditions, metav1.Condition{
		Type:    policyengine.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	return state
}

func workflowInvalid(message string) WorkflowReconcileState {
	state := WorkflowReconcileState{Phase: policyv1alpha1.PolicyWorkflowInvalid}
	meta.SetStatusCondition(&state.Conditions, metav1.Condition{
		Type:    policyengine.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Invalid",
		Message: message,
	})
	return state
}

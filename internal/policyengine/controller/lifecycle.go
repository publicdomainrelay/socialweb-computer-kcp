package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

const (
	DefaultStatusTimeout = 15 * time.Second

	ConditionRunning = "Running"
)

func StartingStatus(run *policyv1alpha1.PolicyWorkflowRun, now time.Time) policyv1alpha1.PolicyWorkflowRunStatus {
	restarts := run.Status.Restarts
	starting := *run.Status.DeepCopy()
	starting.Phase = policyv1alpha1.PolicyWorkflowRunRunning
	starting.Restarts = restarts
	if starting.StartTime == nil {
		started := metav1.NewTime(now)
		starting.StartTime = &started
	}
	conditions := append([]metav1.Condition(nil), starting.Conditions...)
	if run.Status.Phase == policyv1alpha1.PolicyWorkflowRunRunning {
		restarts++
		starting.Restarts = restarts
		meta.SetStatusCondition(&conditions, metav1.Condition{
			Type:    policyengine.ConditionRestarted,
			Status:  metav1.ConditionTrue,
			Reason:  "Restarted",
			Message: "the policy run is executing again",
		})
	}
	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               ConditionRunning,
		Status:             metav1.ConditionTrue,
		Reason:             "Executing",
		Message:            "the policy run is executing",
		ObservedGeneration: run.Generation,
	})
	starting.Conditions = conditions
	return starting
}

func FinalStatus(current *policyv1alpha1.PolicyWorkflowRun, outcome Outcome, now time.Time) policyv1alpha1.PolicyWorkflowRunStatus {
	status := RunStatusFromOutcome(outcome)
	status.Restarts = max(status.Restarts, current.Status.Restarts)
	switch {
	case current.Spec.Suspend:
		suspended := RunStatusFromOutcome(cancelledOutcome(current, policyengine.ConditionSuspended, "Suspended", "the run is suspended"))
		status = preserveResult(suspended, status)
	case current.Spec.Cancel:
		cancelled := RunStatusFromOutcome(cancelledOutcome(current, policyengine.ConditionCancelled, "Cancelled", "the run was cancelled"))
		status = preserveResult(cancelled, status)
	}
	if status.RunID == "" {
		status.RunID = current.Status.RunID
	}
	if status.StartTime == nil {
		status.StartTime = current.Status.StartTime
	}
	if status.StartTime == nil {
		started := metav1.NewTime(now)
		status.StartTime = &started
	}
	if status.CompletionTime == nil {
		completed := metav1.NewTime(now)
		status.CompletionTime = &completed
	}
	if status.Phase == "" {
		status.Phase = policyv1alpha1.PolicyWorkflowRunFailed
		status.ExitStatus = policyv1alpha1.PolicyExitStatusFailure
	}
	return status
}

func CancelledStatus(run *policyv1alpha1.PolicyWorkflowRun) policyv1alpha1.PolicyWorkflowRunStatus {
	return RunStatusFromOutcome(cancelledOutcome(run, policyengine.ConditionCancelled, "Cancelled", "the run was cancelled"))
}

func SuspendedStatus(run *policyv1alpha1.PolicyWorkflowRun) policyv1alpha1.PolicyWorkflowRunStatus {
	return RunStatusFromOutcome(cancelledOutcome(run, policyengine.ConditionSuspended, "Suspended", "the run is suspended"))
}

func DeadlineStatus(run *policyv1alpha1.PolicyWorkflowRun) policyv1alpha1.PolicyWorkflowRunStatus {
	outcome := failedOutcome(nil)
	message := "the policy run exceeded its deadline"
	outcome.Status.Annotations["error"] = []models.Annotation{{AnnotationLevel: models.AnnotationError, Message: message, Title: message}}
	outcome.Status.Conditions = nil
	meta.SetStatusCondition(&outcome.Status.Conditions, metav1.Condition{
		Type:    policyengine.ConditionFailed,
		Status:  metav1.ConditionTrue,
		Reason:  "DeadlineExceeded",
		Message: message,
	})
	status := RunStatusFromOutcome(outcome)
	status.RunID = run.Status.RunID
	status.Restarts = run.Status.Restarts
	status.StartTime = run.Status.StartTime
	return status
}

func PendingStatus(run *policyv1alpha1.PolicyWorkflowRun, reason, message string) policyv1alpha1.PolicyWorkflowRunStatus {
	value := *run.Status.DeepCopy()
	value.Phase = policyv1alpha1.PolicyWorkflowRunPending
	conditions := append([]metav1.Condition(nil), value.Conditions...)
	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               policyengine.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	value.Conditions = conditions
	return value
}

func preserveResult(status, result policyv1alpha1.PolicyWorkflowRunStatus) policyv1alpha1.PolicyWorkflowRunStatus {
	if result.RunID != "" {
		status.RunID = result.RunID
	}
	if result.Outputs != nil {
		status.Outputs = result.Outputs
	}
	if result.Cache != nil {
		status.Cache = result.Cache
	}
	if result.ConsoleOutput != "" {
		status.ConsoleOutput = result.ConsoleOutput
	}
	if result.Annotations != nil {
		status.Annotations = result.Annotations
	}
	return status
}

func RunDeadline(run *policyv1alpha1.PolicyWorkflowRun) (time.Time, bool) {
	if run.Spec.ActiveDeadlineSeconds == nil || *run.Spec.ActiveDeadlineSeconds <= 0 {
		return time.Time{}, false
	}
	start := run.CreationTimestamp.Time
	if run.Status.StartTime != nil {
		start = run.Status.StartTime.Time
	}
	return start.Add(time.Duration(*run.Spec.ActiveDeadlineSeconds) * time.Second), true
}

func RunTTL(run *policyv1alpha1.PolicyWorkflowRun, workflow *policyv1alpha1.PolicyWorkflow, fallback time.Duration) (time.Duration, bool) {
	if run.Spec.TTLSecondsAfterFinished != nil {
		if *run.Spec.TTLSecondsAfterFinished < 0 {
			return 0, false
		}
		return time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second, true
	}
	if workflow != nil && workflow.Spec.TTLSecondsAfterFinished != nil {
		if *workflow.Spec.TTLSecondsAfterFinished < 0 {
			return 0, false
		}
		return time.Duration(*workflow.Spec.TTLSecondsAfterFinished) * time.Second, true
	}
	if fallback > 0 {
		return fallback, true
	}
	return 0, false
}

func RunExpiry(run *policyv1alpha1.PolicyWorkflowRun, ttl time.Duration) time.Time {
	completed := run.Status.CompletionTime
	if completed == nil {
		completed = run.Status.StartTime
	}
	if completed == nil {
		created := run.CreationTimestamp
		completed = &created
	}
	return completed.Add(ttl)
}

func Finished(run *policyv1alpha1.PolicyWorkflowRun) bool {
	return run.Status.Phase == policyv1alpha1.PolicyWorkflowRunSucceeded || run.Status.Phase == policyv1alpha1.PolicyWorkflowRunFailed
}

func (r *TriggerReconciler) DispatchRuns(ctx context.Context, trigger *policyv1alpha1.PolicyTrigger, runs []*policyv1alpha1.PolicyWorkflowRun,
	write func(TriggerReconcileState) error) error {
	finished := make([]*policyv1alpha1.PolicyWorkflowRun, 0, len(runs))
	for _, run := range runs {
		if Finished(run) {
			finished = append(finished, run)
		}
	}
	sort.SliceStable(finished, func(i, j int) bool {
		left := RunCompletion(finished[i])
		right := RunCompletion(finished[j])
		if left.Equal(right) {
			return finished[i].Name < finished[j].Name
		}
		return left.Before(right)
	})
	var lastErr error
	for _, run := range finished {
		completion := metav1.NewTime(RunCompletion(run))
		if !TriggerMatchesRun(trigger, run) {
			continue
		}
		if trigger.Status.LastRun != nil && !completion.After(trigger.Status.LastRun.Time) {
			continue
		}
		state, err := r.Handle(ctx, trigger, OutcomeFromRun(run))
		if err != nil {
			lastErr = err
			continue
		}
		state.LastRun = &completion
		state.NextRun = trigger.Status.NextRun
		conditions := append([]metav1.Condition(nil), trigger.Status.Conditions...)
		for _, condition := range state.Conditions {
			meta.SetStatusCondition(&conditions, condition)
		}
		state.Conditions = conditions
		if err := write(state); err != nil {
			return err
		}
		trigger.Status.LastRun = state.LastRun
		trigger.Status.NextRun = state.NextRun
		trigger.Status.Active = state.Active
		trigger.Status.Conditions = state.Conditions
	}
	return lastErr
}

func TriggerMatchesRun(trigger *policyv1alpha1.PolicyTrigger, run *policyv1alpha1.PolicyWorkflowRun) bool {
	if trigger.DeletionTimestamp != nil || trigger.Spec.Suspend {
		return false
	}
	if !trigger.CreationTimestamp.IsZero() && RunCompletion(run).Before(trigger.CreationTimestamp.Time) {
		return false
	}
	if trigger.Spec.EngineRef != "" && run.Spec.EngineRef != "" && trigger.Spec.EngineRef != run.Spec.EngineRef {
		return false
	}
	if trigger.Spec.WorkflowRef != "" {
		if run.Spec.WorkflowRef != trigger.Spec.WorkflowRef {
			return false
		}
	} else if len(bytes.TrimSpace(trigger.Spec.Workflow)) > 0 {
		if len(bytes.TrimSpace(run.Spec.Workflow)) == 0 {
			return false
		}
		left, leftErr := normalizeWorkflow(trigger.Spec.Workflow)
		right, rightErr := normalizeWorkflow(run.Spec.Workflow)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return false
		}
	}
	return MatchesOutputs(outputsFromAPI(run.Status.Outputs), trigger.Spec.Match)
}

func OutcomeFromRun(run *policyv1alpha1.PolicyWorkflowRun) Outcome {
	status := policyengine.Status{
		Phase:         policyengine.Phase(run.Status.Phase),
		RunID:         run.Status.RunID,
		ExitStatus:    models.ExitStatus(run.Status.ExitStatus),
		Outputs:       outputsFromAPI(run.Status.Outputs),
		Cache:         CacheFromAPI(run.Status.Cache),
		ConsoleOutput: run.Status.ConsoleOutput,
		Annotations:   annotationsFromAPI(run.Status.Annotations),
		Restarts:      run.Status.Restarts,
		StartedAt:     run.Status.StartTime,
		CompletedAt:   run.Status.CompletionTime,
		Conditions:    append([]metav1.Condition(nil), run.Status.Conditions...),
	}
	return Outcome{
		Result: models.Result{
			RunID:         status.RunID,
			Status:        models.ExecutionComplete,
			ExitStatus:    status.ExitStatus,
			Outputs:       status.Outputs,
			Cache:         status.Cache,
			Annotations:   status.Annotations,
			ConsoleOutput: status.ConsoleOutput,
		},
		Status: status,
	}
}

func outputsFromAPI(values map[string]json.RawMessage) models.Outputs {
	out := models.Outputs{}
	for key, raw := range values {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		out[key] = value
	}
	return out
}

func annotationsFromAPI(values map[string]json.RawMessage) models.Annotations {
	out := models.Annotations{}
	for key, raw := range values {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if object, ok := value.(map[string]any); ok && len(object) == 1 {
			if items, exists := object["items"]; exists {
				out[key] = items
				continue
			}
			if single, exists := object["value"]; exists {
				out[key] = single
				continue
			}
		}
		out[key] = value
	}
	return out
}

func RunCompletion(run *policyv1alpha1.PolicyWorkflowRun) time.Time {
	if run.Status.CompletionTime != nil {
		return run.Status.CompletionTime.Time
	}
	if run.Status.StartTime != nil {
		return run.Status.StartTime.Time
	}
	return run.CreationTimestamp.Time
}

func EngineStatusEqual(current policyv1alpha1.PolicyEngineStatus, state EngineReconcileState) bool {
	return current.Phase == state.Phase && current.Ready == state.Ready && current.ActiveRuns == state.ActiveRuns && conditionsMatch(current.Conditions, state.Conditions)
}

func WorkflowStatusEqual(current policyv1alpha1.PolicyWorkflowStatus, state WorkflowReconcileState, generation int64) bool {
	return current.Phase == state.Phase && current.ObservedGeneration == generation && conditionsMatch(current.Conditions, state.Conditions)
}

func TriggerStatusEqual(current policyv1alpha1.PolicyTriggerStatus, state TriggerReconcileState) bool {
	return timePtrEqual(current.LastRun, state.LastRun) && timePtrEqual(current.NextRun, state.NextRun) && current.Active == state.Active && conditionsMatch(current.Conditions, state.Conditions)
}

func RunStatusEqual(current, next policyv1alpha1.PolicyWorkflowRunStatus) bool {
	if current.Phase != next.Phase || current.RunID != next.RunID || current.ExitStatus != next.ExitStatus || current.Restarts != next.Restarts {
		return false
	}
	if current.ConsoleOutput != next.ConsoleOutput {
		return false
	}
	if !timePtrEqual(current.StartTime, next.StartTime) || !timePtrEqual(current.CompletionTime, next.CompletionTime) {
		return false
	}
	if !reflect.DeepEqual(current.Outputs, next.Outputs) || !reflect.DeepEqual(current.Cache, next.Cache) || !reflect.DeepEqual(current.Annotations, next.Annotations) {
		return false
	}
	return conditionsMatch(current.Conditions, next.Conditions)
}

func conditionsMatch(left, right []metav1.Condition) bool {
	if len(left) != len(right) {
		return false
	}
	normalize := func(values []metav1.Condition) []metav1.Condition {
		out := make([]metav1.Condition, len(values))
		copy(out, values)
		for index := range out {
			out[index].LastTransitionTime = metav1.Time{}
		}
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].Type < out[j].Type
		})
		return out
	}
	return reflect.DeepEqual(normalize(left), normalize(right))
}

func timePtrEqual(left, right *metav1.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Time.Equal(right.Time)
}

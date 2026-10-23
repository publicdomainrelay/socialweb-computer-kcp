package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

const (
	DefaultPollInterval = 2 * time.Second

	DefaultStatusTimeout = 15 * time.Second

	conditionRunning = "Running"
)

type ControllerConfig struct {
	Resources *Resources

	Manager *EngineManager

	Workflows *WorkflowReconciler

	Runs *RunReconciler

	Triggers *TriggerReconciler

	Log *slog.Logger

	PollInterval time.Duration

	DefaultRunTTL time.Duration

	Now func() time.Time
}

type activeRun struct {
	uid types.UID

	cancel context.CancelFunc
}

type Controller struct {
	cfg ControllerConfig

	log *slog.Logger

	now func() time.Time

	mu sync.Mutex

	active map[string]*activeRun

	terminal map[string]policyv1alpha1.PolicyWorkflowRunStatus

	wg sync.WaitGroup
}

func NewController(cfg ControllerConfig) (*Controller, error) {
	switch {
	case cfg.Resources == nil:
		return nil, errors.New("policy controller: resources are required")
	case cfg.Manager == nil:
		return nil, errors.New("policy controller: the engine manager is required")
	case cfg.Runs == nil:
		return nil, errors.New("policy controller: the run reconciler is required")
	}
	if cfg.Workflows == nil {
		cfg.Workflows = &WorkflowReconciler{}
	}
	if cfg.Triggers == nil {
		cfg.Triggers = &TriggerReconciler{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{
		cfg:      cfg,
		log:      cfg.Log,
		now:      cfg.Now,
		active:   map[string]*activeRun{},
		terminal: map[string]policyv1alpha1.PolicyWorkflowRunStatus{},
	}, nil
}

func (c *Controller) Run(ctx context.Context) error {
	if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
		c.log.Warn("policy controller reconcile failed", "err", err.Error())
	}
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.cancelAll()
			c.wg.Wait()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultStatusTimeout)
			defer cancel()
			if err := c.cfg.Manager.Shutdown(shutdownCtx); err != nil {
				c.log.Warn("policy engine shutdown failed", "err", err.Error())
			}
			return nil
		case <-ticker.C:
			if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("policy controller reconcile failed", "err", err.Error())
			}
		}
	}
}

func (c *Controller) Engine(name string) (*Engine, bool) {
	return c.cfg.Manager.EngineFor(name)
}

func (c *Controller) Reconcile(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	var errs []error
	engines, err := c.cfg.Resources.Engines.List(ctx, c.cfg.Resources.Cluster)
	if err != nil {
		errs = append(errs, fmt.Errorf("list policy engines: %w", err))
	} else {
		c.reconcileEngines(ctx, engines)
	}
	workflows, err := c.cfg.Resources.Workflows.List(ctx, c.cfg.Resources.Cluster)
	if err != nil {
		errs = append(errs, fmt.Errorf("list policy workflows: %w", err))
	} else {
		c.reconcileWorkflows(ctx, workflows)
	}
	runs, runErr := c.cfg.Resources.Runs.List(ctx, c.cfg.Resources.Cluster)
	if runErr != nil {
		errs = append(errs, fmt.Errorf("list policy workflow runs: %w", runErr))
	} else {
		c.reconcileRuns(ctx, runs)
	}
	triggers, triggerErr := c.cfg.Resources.Triggers.List(ctx, c.cfg.Resources.Cluster)
	if triggerErr != nil {
		errs = append(errs, fmt.Errorf("list policy triggers: %w", triggerErr))
	} else {
		c.reconcileTriggers(ctx, triggers, runs)
	}
	return errors.Join(errs...)
}

func (c *Controller) reconcileEngines(ctx context.Context, engines []policyv1alpha1.PolicyEngine) {
	seen := make(map[string]bool, len(engines))
	for index := range engines {
		engine := &engines[index]
		seen[engine.Name] = true
		state, err := c.cfg.Manager.ReconcilePolicyEngine(ctx, engine)
		if err != nil {
			c.log.Warn("reconciling a policy engine", "engine", engine.Name, "err", err.Error())
		}
		if engineStatusEqual(engine.Status, state) {
			continue
		}
		if err := c.cfg.Resources.WriteEngineStatus(ctx, engine, state); err != nil && !kcpstore.IsNotFound(err) {
			c.log.Warn("writing policy engine status", "engine", engine.Name, "err", err.Error())
		}
	}
	for _, name := range c.cfg.Manager.Names() {
		if seen[name] {
			continue
		}
		if err := c.cfg.Manager.Delete(ctx, name); err != nil {
			c.log.Warn("stopping a deleted policy engine", "engine", name, "err", err.Error())
		}
	}
}

func (c *Controller) reconcileWorkflows(ctx context.Context, workflows []policyv1alpha1.PolicyWorkflow) {
	for index := range workflows {
		workflow := &workflows[index]
		state, err := c.cfg.Workflows.ReconcilePolicyWorkflow(ctx, workflow)
		if err != nil {
			c.log.Warn("reconciling a policy workflow", "workflow", workflow.Name, "err", err.Error())
			continue
		}
		if workflowStatusEqual(workflow.Status, state, workflow.Generation) {
			continue
		}
		if err := c.cfg.Resources.WriteWorkflowStatus(ctx, workflow, state); err != nil && !kcpstore.IsNotFound(err) {
			c.log.Warn("writing policy workflow status", "workflow", workflow.Name, "err", err.Error())
		}
	}
}

func (c *Controller) reconcileRuns(ctx context.Context, runs []policyv1alpha1.PolicyWorkflowRun) {
	seen := make(map[string]bool, len(runs))
	for index := range runs {
		c.reconcileRun(ctx, &runs[index], seen)
	}
	c.cancelUnseen(seen)
}

func (c *Controller) reconcileRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, seen map[string]bool) {
	key := runKey(run)
	seen[key] = true
	if run.DeletionTimestamp != nil {
		c.cancelActive(key)
		c.forgetTerminal(key)
		return
	}
	if isTerminalPhase(run.Status.Phase) {
		c.cancelActive(key)
		c.forgetTerminal(key)
		c.maybeDeleteRun(ctx, run)
		return
	}
	if run.Spec.Cancel {
		c.forgetTerminal(key)
		if !c.isActive(key) {
			outcome := cancelledOutcome(run, policyengine.ConditionCancelled, "Cancelled", "the run was cancelled")
			c.writeRunStatus(ctx, run, RunStatusFromOutcome(outcome))
		}
		c.cancelActive(key)
		return
	}
	if run.Spec.Suspend {
		c.forgetTerminal(key)
		if !c.isActive(key) {
			outcome := cancelledOutcome(run, policyengine.ConditionSuspended, "Suspended", "the run is suspended")
			c.writeRunStatus(ctx, run, RunStatusFromOutcome(outcome))
		}
		c.cancelActive(key)
		return
	}
	if status, ok := c.terminalStatus(key); ok {
		c.cancelActive(key)
		if err := c.writeRunStatus(ctx, run, status); err == nil {
			c.forgetTerminal(key)
		}
		return
	}
	if c.isActive(key) {
		return
	}
	engine, ok := c.engineFor(run.Spec.EngineRef)
	if !ok {
		reason := "EngineUnavailable"
		message := "the run is waiting for a ready policy engine"
		if run.Spec.EngineRef != "" {
			message = fmt.Sprintf("the run is waiting for policy engine %s", run.Spec.EngineRef)
		}
		c.writeRunPending(ctx, run, reason, message)
		return
	}
	c.startRun(ctx, run, engine)
}

func (c *Controller) engineFor(name string) (*Engine, bool) {
	if name != "" {
		engine, ok := c.cfg.Manager.EngineFor(name)
		if !ok {
			return nil, false
		}
		return engine, engine.Ready()
	}
	names := c.cfg.Manager.Names()
	if len(names) != 1 {
		return nil, false
	}
	engine, ok := c.cfg.Manager.EngineFor(names[0])
	if !ok {
		return nil, false
	}
	return engine, engine.Ready()
}

func (c *Controller) startRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, engine *Engine) {
	key := runKey(run)
	restarts := run.Status.Restarts
	starting := run.Status
	starting.Phase = policyv1alpha1.PolicyWorkflowRunRunning
	starting.Restarts = restarts
	if starting.StartTime == nil {
		now := metav1.NewTime(c.now())
		starting.StartTime = &now
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
		Type:               conditionRunning,
		Status:             metav1.ConditionTrue,
		Reason:             "Executing",
		Message:            "the policy run is executing",
		ObservedGeneration: run.Generation,
	})
	starting.Conditions = conditions

	runCtx, cancel := context.WithCancel(ctx)
	handle := &activeRun{uid: run.UID, cancel: cancel}
	c.mu.Lock()
	c.active[key] = handle
	c.mu.Unlock()
	c.writeRunStatus(ctx, run, starting)
	runCopy := run.DeepCopy()
	runCopy.Status = starting
	c.wg.Go(func() {
		reconciler := *c.cfg.Runs
		reconciler.Engine = engine
		outcome, err := reconciler.ReconcilePolicyWorkflowRun(runCtx, runCopy)
		if err != nil {
			c.log.Warn("executing a policy run", "run", key, "err", err.Error())
		}
		c.finalizeRun(runCtx, key, handle, runCopy, outcome)
	})
}

func (c *Controller) finalizeRun(runCtx context.Context, key string, handle *activeRun, run *policyv1alpha1.PolicyWorkflowRun, outcome Outcome) {
	defer c.forgetActive(key, handle)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), DefaultStatusTimeout)
	defer cancel()
	current, err := c.cfg.Resources.Runs.Get(ctx, c.cfg.Resources.Cluster, run.Name)
	if err != nil {
		if !kcpstore.IsNotFound(err) {
			c.log.Warn("reading a policy run after execution", "run", key, "err", err.Error())
		}
		return
	}
	if current.DeletionTimestamp != nil {
		return
	}
	status := RunStatusFromOutcome(outcome)
	status.Restarts = maxInt32(status.Restarts, current.Status.Restarts)
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
		now := metav1.NewTime(c.now())
		status.StartTime = &now
	}
	if status.CompletionTime == nil {
		now := metav1.NewTime(c.now())
		status.CompletionTime = &now
	}
	if status.Phase == "" {
		status.Phase = policyv1alpha1.PolicyWorkflowRunFailed
		status.ExitStatus = policyv1alpha1.PolicyExitStatusFailure
	}
	if err := c.writeRunStatus(ctx, current, status); err != nil {
		c.rememberTerminal(key, status)
	} else {
		c.forgetTerminal(key)
	}
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

func (c *Controller) writeRunPending(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, reason, message string) {
	value := run.Status
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
	c.writeRunStatus(ctx, run, value)
}

func (c *Controller) writeRunStatus(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun, value policyv1alpha1.PolicyWorkflowRunStatus) error {
	if run.DeletionTimestamp != nil {
		return nil
	}
	if runStatusEqual(run.Status, value) {
		return nil
	}
	if err := c.cfg.Resources.WriteRunStatus(ctx, run, value); err != nil {
		if kcpstore.IsNotFound(err) {
			return nil
		}
		c.log.Warn("writing policy run status", "run", runKey(run), "err", err.Error())
		return err
	}
	return nil
}

func (c *Controller) maybeDeleteRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) {
	ttl, ok := c.runTTL(ctx, run)
	if !ok {
		return
	}
	completed := run.Status.CompletionTime
	if completed == nil {
		completed = run.Status.StartTime
	}
	if completed == nil {
		created := run.CreationTimestamp
		completed = &created
	}
	if c.now().Before(completed.Add(ttl)) {
		return
	}
	if err := c.cfg.Resources.DeleteRun(ctx, run); err != nil && !kcpstore.IsNotFound(err) {
		c.log.Warn("deleting a finished policy run", "run", runKey(run), "err", err.Error())
	}
}

func (c *Controller) runTTL(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) (time.Duration, bool) {
	if run.Spec.TTLSecondsAfterFinished != nil {
		if *run.Spec.TTLSecondsAfterFinished < 0 {
			return 0, false
		}
		return time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second, true
	}
	if run.Spec.WorkflowRef != "" && c.cfg.Runs.Workflows != nil {
		if workflow, err := c.cfg.Runs.Workflows.GetPolicyWorkflow(ctx, run.Spec.WorkflowRef); err == nil && workflow != nil {
			if workflow.Spec.TTLSecondsAfterFinished != nil {
				if *workflow.Spec.TTLSecondsAfterFinished < 0 {
					return 0, false
				}
				return time.Duration(*workflow.Spec.TTLSecondsAfterFinished) * time.Second, true
			}
		}
	}
	if c.cfg.DefaultRunTTL > 0 {
		return c.cfg.DefaultRunTTL, true
	}
	return 0, false
}

func (c *Controller) reconcileTriggers(ctx context.Context, triggers []policyv1alpha1.PolicyTrigger, runs []policyv1alpha1.PolicyWorkflowRun) {
	for index := range triggers {
		trigger := &triggers[index]
		state, err := c.cfg.Triggers.ReconcilePolicyTrigger(ctx, trigger)
		if err != nil {
			c.log.Warn("reconciling a policy trigger", "trigger", trigger.Name, "err", err.Error())
			continue
		}
		state.LastRun = trigger.Status.LastRun
		state.NextRun = trigger.Status.NextRun
		if triggerStatusEqual(trigger.Status, state) {
			continue
		}
		if err := c.cfg.Resources.WriteTriggerStatus(ctx, trigger, state); err != nil && !kcpstore.IsNotFound(err) {
			c.log.Warn("writing policy trigger status", "trigger", trigger.Name, "err", err.Error())
		}
	}
	if len(triggers) == 0 || len(runs) == 0 {
		return
	}
	finished := make([]*policyv1alpha1.PolicyWorkflowRun, 0, len(runs))
	for index := range runs {
		run := &runs[index]
		if run.Status.Phase != policyv1alpha1.PolicyWorkflowRunSucceeded && run.Status.Phase != policyv1alpha1.PolicyWorkflowRunFailed {
			continue
		}
		finished = append(finished, run)
	}
	sort.SliceStable(finished, func(i, j int) bool {
		left := runCompletion(finished[i])
		right := runCompletion(finished[j])
		if left.Equal(right) {
			return finished[i].Name < finished[j].Name
		}
		return left.Before(right)
	})
	for _, run := range finished {
		completion := metav1.NewTime(runCompletion(run))
		outputs := outputsFromAPI(run.Status.Outputs)
		for index := range triggers {
			trigger := &triggers[index]
			if !triggerMatchesRun(trigger, run) {
				continue
			}
			if trigger.Status.LastRun != nil && !completion.After(trigger.Status.LastRun.Time) {
				continue
			}
			if !MatchesOutputs(outputs, trigger.Spec.Match) {
				continue
			}
			outcome := outcomeFromRun(run)
			reconciler := *c.cfg.Triggers
			state, err := reconciler.Handle(ctx, trigger, outcome)
			if err != nil {
				c.log.Warn("dispatching a policy trigger", "trigger", trigger.Name, "run", runKey(run), "err", err.Error())
				continue
			}
			state.LastRun = &completion
			state.NextRun = trigger.Status.NextRun
			conditions := append([]metav1.Condition(nil), trigger.Status.Conditions...)
			for _, condition := range state.Conditions {
				meta.SetStatusCondition(&conditions, condition)
			}
			state.Conditions = conditions
			if err := c.cfg.Resources.WriteTriggerStatus(ctx, trigger, state); err != nil && !kcpstore.IsNotFound(err) {
				c.log.Warn("writing policy trigger status", "trigger", trigger.Name, "err", err.Error())
				continue
			}
			trigger.Status.LastRun = state.LastRun
			trigger.Status.NextRun = state.NextRun
			trigger.Status.Active = state.Active
			trigger.Status.Conditions = state.Conditions
		}
	}
}

func triggerMatchesRun(trigger *policyv1alpha1.PolicyTrigger, run *policyv1alpha1.PolicyWorkflowRun) bool {
	if trigger.DeletionTimestamp != nil || trigger.Spec.Suspend {
		return false
	}
	if !trigger.CreationTimestamp.IsZero() && runCompletion(run).Before(trigger.CreationTimestamp.Time) {
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

func outcomeFromRun(run *policyv1alpha1.PolicyWorkflowRun) Outcome {
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

func runCompletion(run *policyv1alpha1.PolicyWorkflowRun) time.Time {
	if run.Status.CompletionTime != nil {
		return run.Status.CompletionTime.Time
	}
	if run.Status.StartTime != nil {
		return run.Status.StartTime.Time
	}
	return run.CreationTimestamp.Time
}

func runKey(run *policyv1alpha1.PolicyWorkflowRun) string {
	return run.Namespace + "/" + run.Name
}

func isTerminalPhase(phase policyv1alpha1.PolicyWorkflowRunPhase) bool {
	switch phase {
	case policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed, policyv1alpha1.PolicyWorkflowRunCancelled:
		return true
	default:
		return false
	}
}

func (c *Controller) isActive(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.active[key]
	return ok
}

func (c *Controller) cancelActive(key string) {
	c.mu.Lock()
	handle := c.active[key]
	delete(c.active, key)
	c.mu.Unlock()
	if handle != nil {
		handle.cancel()
	}
}

func (c *Controller) forgetActive(key string, handle *activeRun) {
	c.mu.Lock()
	if c.active[key] == handle {
		delete(c.active, key)
	}
	c.mu.Unlock()
}

func (c *Controller) cancelUnseen(seen map[string]bool) {
	c.mu.Lock()
	handles := make([]*activeRun, 0)
	for key, handle := range c.active {
		if seen[key] {
			continue
		}
		delete(c.active, key)
		handles = append(handles, handle)
	}
	for key := range c.terminal {
		if seen[key] {
			continue
		}
		delete(c.terminal, key)
	}
	c.mu.Unlock()
	for _, handle := range handles {
		handle.cancel()
	}
}

func (c *Controller) rememberTerminal(key string, status policyv1alpha1.PolicyWorkflowRunStatus) {
	c.mu.Lock()
	c.terminal[key] = status
	c.mu.Unlock()
}

func (c *Controller) terminalStatus(key string) (policyv1alpha1.PolicyWorkflowRunStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.terminal[key]
	return status, ok
}

func (c *Controller) forgetTerminal(key string) {
	c.mu.Lock()
	delete(c.terminal, key)
	c.mu.Unlock()
}

func (c *Controller) cancelAll() {
	c.mu.Lock()
	handles := make([]*activeRun, 0, len(c.active))
	for key, handle := range c.active {
		delete(c.active, key)
		handles = append(handles, handle)
	}
	c.mu.Unlock()
	for _, handle := range handles {
		handle.cancel()
	}
}

func engineStatusEqual(current policyv1alpha1.PolicyEngineStatus, state EngineReconcileState) bool {
	return current.Phase == state.Phase && current.Ready == state.Ready && current.ActiveRuns == state.ActiveRuns && conditionsMatch(current.Conditions, state.Conditions)
}

func workflowStatusEqual(current policyv1alpha1.PolicyWorkflowStatus, state WorkflowReconcileState, generation int64) bool {
	return current.Phase == state.Phase && current.ObservedGeneration == generation && conditionsMatch(current.Conditions, state.Conditions)
}

func triggerStatusEqual(current policyv1alpha1.PolicyTriggerStatus, state TriggerReconcileState) bool {
	return timePtrEqual(current.LastRun, state.LastRun) && timePtrEqual(current.NextRun, state.NextRun) && current.Active == state.Active && conditionsMatch(current.Conditions, state.Conditions)
}

func runStatusEqual(current, next policyv1alpha1.PolicyWorkflowRunStatus) bool {
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

func maxInt32(left, right int32) int32 {
	if left > right {
		return left
	}
	return right
}

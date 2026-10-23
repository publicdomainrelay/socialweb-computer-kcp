package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/workflow"
)

type PolicyEngineRequest = models.Request

type Limits struct {
	MaxConcurrentRuns int
	QueueDepth        int
	ActiveDeadline    time.Duration
	MaxConsoleBytes   int64
	MaxOutputBytes    int64
	MaxCacheEntries   int
	MaxCacheBytes     int64
}

type Config struct {
	Limits           Limits
	DenoPath         string
	ResolveAction    workflow.ActionResolver
	Host             trust.Host
	DisableActions   bool
	VerdictTTL       time.Duration
	ScopeTTL         time.Duration
	NegativeScopeTTL time.Duration
	RecordTTL        time.Duration
	TrustTTL         time.Duration
	Log              func(level, msg string, meta map[string]any)
}

type RunOptions struct {
	Deadline time.Duration
	Cache    models.Cache
	TTL      time.Duration
}

type Outcome struct {
	Result models.Result
	Status policyengine.Status
}

type runState struct {
	mu      sync.Mutex
	status  policyengine.Status
	outcome Outcome
	done    chan struct{}
	cancel  context.CancelFunc
	closed  bool
}

type Engine struct {
	config     Config
	resolver   workflow.ActionResolver
	cacheStore *cache.Store
	log        func(level, msg string, meta map[string]any)
	sem        chan struct{}
	queueDepth int

	mu       sync.Mutex
	runs     map[string]*runState
	rootCtx  context.Context
	cancel   context.CancelFunc
	started  bool
	stopping bool
	wg       sync.WaitGroup
	active   atomic.Int32
	waiting  atomic.Int32
}

func NewEngine(config Config) *Engine {
	if config.Limits.MaxConcurrentRuns <= 0 {
		config.Limits.MaxConcurrentRuns = 4
	}
	if config.Limits.QueueDepth <= 0 {
		config.Limits.QueueDepth = 64
	}
	if config.Limits.MaxCacheEntries <= 0 {
		config.Limits.MaxCacheEntries = 512
	}
	if config.Limits.MaxCacheBytes < 0 {
		config.Limits.MaxCacheBytes = 0
	}
	denoPath := config.DenoPath
	if denoPath == "" {
		if path, err := deno.Resolve(""); err == nil {
			denoPath = path
		}
	}
	resolver := config.ResolveAction
	if config.DisableActions {
		resolver = func(uses string) (*workflow.Action, error) {
			return nil, fmt.Errorf("bundled actions are disabled")
		}
	} else if resolver == nil {
		resolver = workflow.ActionResolver(defaultActionResolver)
	}
	log := config.Log
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	config.DenoPath = denoPath
	return &Engine{
		config:     config,
		resolver:   resolver,
		cacheStore: cache.NewStore(config.Limits.MaxCacheEntries, config.Limits.MaxCacheBytes),
		log:        log,
		sem:        make(chan struct{}, config.Limits.MaxConcurrentRuns),
		queueDepth: config.Limits.QueueDepth,
		runs:       make(map[string]*runState),
		rootCtx:    rootCtx,
		cancel:     cancel,
	}
}

func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}
	rootCtx, cancel := context.WithCancel(ctx)
	e.rootCtx = rootCtx
	e.cancel = cancel
	e.started = true
	e.stopping = false
	return nil
}

func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	if !e.started && e.rootCtx == nil {
		e.mu.Unlock()
		return nil
	}
	e.started = false
	e.stopping = true
	cancel := e.cancel
	runs := make([]*runState, 0, len(e.runs))
	for _, run := range e.runs {
		runs = append(runs, run)
	}
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, run := range runs {
		run.mu.Lock()
		runCancel := run.cancel
		run.mu.Unlock()
		if runCancel != nil {
			runCancel()
		}
	}
	return supervise.Join(ctx, e.wg.Wait)
}

func (e *Engine) Ready() bool {
	return e.config.DenoPath != ""
}

func (e *Engine) ActiveRuns() int32 {
	return e.active.Load()
}

func (e *Engine) QueueDepth() int32 {
	return e.waiting.Load()
}

func (e *Engine) Cache() *cache.Store {
	return e.cacheStore
}

func (e *Engine) Execute(ctx context.Context, request *PolicyEngineRequest, opts RunOptions) (Outcome, error) {
	if request == nil {
		return Outcome{}, fmt.Errorf("policy engine request is required")
	}
	if err := request.Validate(); err != nil {
		return Outcome{}, err
	}
	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		return Outcome{}, fmt.Errorf("policy engine is shutting down")
	}
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	runID := uuid.NewString()
	state := e.registerRun(runID)
	outcome := e.executeWithID(ctx, runID, request, opts)
	e.finishRun(state, outcome)
	return outcome, nil
}

func (e *Engine) Submit(ctx context.Context, request *PolicyEngineRequest, opts RunOptions) (string, error) {
	if request == nil {
		return "", fmt.Errorf("policy engine request is required")
	}
	if err := request.Validate(); err != nil {
		return "", err
	}
	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		return "", fmt.Errorf("policy engine is shutting down")
	}
	e.wg.Add(1)
	e.mu.Unlock()
	runID := uuid.NewString()
	state := e.registerRun(runID)
	base := e.rootContext()
	runCtx, cancel := context.WithCancel(base)
	state.mu.Lock()
	state.cancel = cancel
	state.mu.Unlock()
	go func() {
		defer e.wg.Done()
		outcome := e.executeWithID(runCtx, runID, request, opts)
		e.finishRun(state, outcome)
		cancel()
	}()
	return runID, nil
}

func (e *Engine) Wait(ctx context.Context, runID string) (Outcome, error) {
	e.mu.Lock()
	state, ok := e.runs[runID]
	e.mu.Unlock()
	if !ok {
		return Outcome{}, fmt.Errorf("run %s not found", runID)
	}
	select {
	case <-state.done:
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.outcome, nil
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
}

func (e *Engine) Cancel(runID string) bool {
	e.mu.Lock()
	state, ok := e.runs[runID]
	e.mu.Unlock()
	if !ok {
		return false
	}
	state.mu.Lock()
	cancel := state.cancel
	state.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (e *Engine) RunStatus(runID string) (policyengine.Status, bool) {
	e.mu.Lock()
	state, ok := e.runs[runID]
	e.mu.Unlock()
	if !ok {
		return policyengine.Status{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.status, true
}

func (e *Engine) executeWithID(ctx context.Context, runID string, request *PolicyEngineRequest, opts RunOptions) Outcome {
	startedAt := metav1.Now()
	timeout := opts.Deadline
	if limit := e.config.Limits.ActiveDeadline; limit > 0 && (timeout <= 0 || limit < timeout) {
		timeout = limit
	}
	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	deadline := metav1.NewTime(time.Now().Add(timeout))
	if timeout <= 0 {
		deadline = metav1.Time{}
	}
	if err := e.acquire(runCtx); err != nil {
		statusValue := policyengine.Status{
			Phase:     policyengine.PhaseCancelled,
			RunID:     runID,
			StartedAt: &startedAt,
		}
		e.setCondition(&statusValue, policyengine.ConditionCancelled, metav1.ConditionTrue, "Cancelled", "the run was cancelled before execution")
		completed := metav1.Now()
		statusValue.CompletedAt = &completed
		return Outcome{Status: statusValue}
	}
	defer e.release()
	e.active.Add(1)
	defer e.active.Add(-1)
	seed := e.seedRequest(request, opts.Cache)
	executor := workflow.New(workflow.Options{
		DenoPath:        e.config.DenoPath,
		ResolveAction:   e.resolver,
		MaxConsoleBytes: e.config.Limits.MaxConsoleBytes,
		MaxOutputBytes:  e.config.Limits.MaxOutputBytes,
	})
	result, runErr := executor.Execute(runCtx, seed)
	completed := metav1.Now()
	statusValue := policyengine.Status{
		RunID:       runID,
		StartedAt:   &startedAt,
		CompletedAt: &completed,
	}
	if !deadline.IsZero() {
		statusValue.Deadline = &deadline
	}
	if result != nil {
		statusValue.ExitStatus = result.ExitStatus
		statusValue.Outputs = result.Outputs
		statusValue.Cache = result.Cache
		statusValue.ConsoleOutput = truncateConsole(result.ConsoleOutput, e.config.Limits.MaxConsoleBytes)
		statusValue.Annotations = result.Annotations
		e.cacheStore.Merge(result.Cache)
	}
	outcome := Outcome{Status: statusValue}
	if result != nil {
		outcome.Result = *result
		outcome.Result.RunID = runID
		outcome.Result.ConsoleOutput = statusValue.ConsoleOutput
		outcome.Result.Cache = statusValue.Cache
	}
	switch {
	case ctx.Err() != nil:
		statusValue.Phase = policyengine.PhaseCancelled
		e.setCondition(&statusValue, policyengine.ConditionCancelled, metav1.ConditionTrue, "Cancelled", "the run context was cancelled")
	case runCtx.Err() == context.DeadlineExceeded:
		statusValue.Phase = policyengine.PhaseFailed
		statusValue.ExitStatus = models.ExitFailure
		e.addErrorAnnotation(&statusValue, "the policy run exceeded its deadline")
		e.setCondition(&statusValue, policyengine.ConditionFailed, metav1.ConditionTrue, "DeadlineExceeded", "the policy run exceeded its deadline")
	case runErr != nil:
		statusValue.Phase = policyengine.PhaseFailed
		statusValue.ExitStatus = models.ExitFailure
		e.addErrorAnnotation(&statusValue, runErr.Error())
		e.setCondition(&statusValue, policyengine.ConditionFailed, metav1.ConditionTrue, "Error", runErr.Error())
	case result == nil:
		statusValue.Phase = policyengine.PhaseFailed
		statusValue.ExitStatus = models.ExitFailure
		e.addErrorAnnotation(&statusValue, "the workflow produced no result")
		e.setCondition(&statusValue, policyengine.ConditionFailed, metav1.ConditionTrue, "NoResult", "the workflow produced no result")
	case result.ExitStatus == models.ExitSuccess:
		statusValue.Phase = policyengine.PhaseSucceeded
		e.setCondition(&statusValue, policyengine.ConditionComplete, metav1.ConditionTrue, "Complete", "the policy run completed successfully")
	default:
		statusValue.Phase = policyengine.PhaseFailed
		statusValue.ExitStatus = models.ExitFailure
		e.setCondition(&statusValue, policyengine.ConditionFailed, metav1.ConditionTrue, "Failed", "the policy run failed")
	}
	outcome.Status = statusValue
	outcome.Result.Status = models.ExecutionComplete
	outcome.Result.ExitStatus = statusValue.ExitStatus
	outcome.Result.Outputs = statusValue.Outputs
	outcome.Result.Cache = statusValue.Cache
	outcome.Result.Annotations = statusValue.Annotations
	outcome.Result.ConsoleOutput = statusValue.ConsoleOutput
	e.updateRunStatus(runID, statusValue)
	e.log("info", "policy run completed", map[string]any{"runID": runID, "phase": string(statusValue.Phase)})
	return outcome
}

func (e *Engine) seedRequest(request *PolicyEngineRequest, seed models.Cache) *PolicyEngineRequest {
	copyRequest := *request
	context := make(map[string]any, len(request.Context)+1)
	maps.Copy(context, request.Context)
	merged := e.cacheStore.Snapshot()
	if existing, ok := cacheFromAny(context["cache"]); ok {
		maps.Copy(merged, existing)
	}
	maps.Copy(merged, seed)
	context["cache"] = merged
	copyRequest.Context = context
	e.cacheStore.Merge(merged)
	return &copyRequest
}

func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		return nil
	default:
	}
	if e.queueDepth > 0 && int(e.waiting.Load()) >= e.queueDepth {
		return fmt.Errorf("policy engine queue is full")
	}
	e.waiting.Add(1)
	defer e.waiting.Add(-1)
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) release() {
	select {
	case <-e.sem:
	default:
	}
}

func (e *Engine) rootContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rootCtx
}

func (e *Engine) registerRun(runID string) *runState {
	state := &runState{
		status: policyengine.Status{Phase: policyengine.PhasePending, RunID: runID},
		done:   make(chan struct{}),
	}
	e.mu.Lock()
	e.runs[runID] = state
	e.mu.Unlock()
	return state
}

func (e *Engine) finishRun(state *runState, outcome Outcome) {
	state.mu.Lock()
	state.outcome = outcome
	state.status = outcome.Status
	if !state.closed {
		state.closed = true
		close(state.done)
	}
	state.mu.Unlock()
}

func (e *Engine) updateRunStatus(runID string, value policyengine.Status) {
	e.mu.Lock()
	state := e.runs[runID]
	e.mu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.status = value
	state.mu.Unlock()
}

func (e *Engine) setCondition(value *policyengine.Status, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&value.Conditions, metav1.Condition{
		Type:    conditionType,
		Status:  conditionStatus,
		Reason:  reason,
		Message: message,
	})
}

func (e *Engine) addErrorAnnotation(value *policyengine.Status, message string) {
	if value.Annotations == nil {
		value.Annotations = models.Annotations{}
	}
	value.Annotations["error"] = []models.Annotation{{
		AnnotationLevel: models.AnnotationError,
		Message:         message,
		Title:           message,
	}}
}

func cacheFromAny(value any) (models.Cache, bool) {
	if value == nil {
		return nil, false
	}
	if typed, ok := value.(models.Cache); ok {
		return typed, true
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var parsed models.Cache
	if err := json.Unmarshal(data, &parsed); err != nil || parsed == nil {
		return nil, false
	}
	return parsed, true
}

func truncateConsole(value string, limit int64) string {
	if limit <= 0 || int64(len(value)) <= limit {
		return value
	}
	return value[:limit]
}

func defaultActionResolver(uses string) (*workflow.Action, error) {
	return executor.BundledActions(uses)
}

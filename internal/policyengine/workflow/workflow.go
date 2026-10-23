package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

type Action struct {
	Uses   string
	Path   string
	Using  string
	Main   string
	Inputs map[string]string
	Source []byte
}

type ActionResolver func(uses string) (*Action, error)

type Options struct {
	DenoPath        string
	ResolveAction   ActionResolver
	MaxConsoleBytes int64
	MaxOutputBytes  int64
}

type JobResult struct {
	Status  string
	Outputs map[string]any
	Error   string
}

type Context struct {
	Inputs      map[string]any
	Env         map[string]any
	Secrets     map[string]string
	Outputs     map[string]map[string]any
	Annotations map[string][]models.Annotation
	Cache       models.Cache
	Vars        map[string]any
	JobResults  map[string]JobResult
	Console     []string
	Shell       string
	Error       error
	console     int64
}

type Executor struct {
	opts        Options
	Context     *Context
	jobFailed   bool
	jobDeadline time.Time
}

func New(options Options) *Executor {
	if options.DenoPath == "" {
		if path, err := deno.Resolve(""); err == nil {
			options.DenoPath = path
		}
	}
	return &Executor{
		opts: options,
		Context: &Context{
			Inputs:      map[string]any{},
			Env:         map[string]any{},
			Secrets:     map[string]string{},
			Outputs:     map[string]map[string]any{},
			Annotations: map[string][]models.Annotation{},
			Cache:       models.Cache{},
			Vars:        map[string]any{},
			JobResults:  map[string]JobResult{},
			Shell:       "bash -xe",
		},
	}
}

func Parse(raw json.RawMessage) (*models.Workflow, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("workflow is required")
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" || text == "null" {
			return nil, fmt.Errorf("workflow is required")
		}
		var parsed models.Workflow
		if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
			return nil, fmt.Errorf("failed to parse workflow YAML: %w", err)
		}
		return &parsed, nil
	}
	var parsed models.Workflow
	if err := yaml.Unmarshal(trimmed, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse workflow: %w", err)
	}
	return &parsed, nil
}

func HasRunStep(raw json.RawMessage) (string, bool) {
	parsed, err := Parse(raw)
	if err != nil {
		return "", false
	}
	names := make([]string, 0, len(parsed.Jobs))
	for name := range parsed.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, step := range parsed.Jobs[name].Steps {
			if strings.TrimSpace(step.Run) != "" {
				return name, true
			}
		}
	}
	return "", false
}

func (e *Executor) Execute(ctx context.Context, request *models.Request) (*models.Result, error) {
	if request == nil {
		return nil, fmt.Errorf("request is required")
	}
	workflow, err := Parse(request.Workflow)
	if err != nil {
		return nil, err
	}
	if err := e.initializeContext(request); err != nil {
		return nil, err
	}
	if workflow.Name != "" {
		e.Context.Env["GITHUB_WORKFLOW"] = workflow.Name
	}
	for key, value := range workflow.Env {
		evaluated, evalErr := e.evaluateExpression(ctx, fmt.Sprintf("%v", value))
		if evalErr != nil {
			return e.failureResult(evalErr), nil
		}
		e.Context.Env[key] = evaluated
	}
	order, err := jobOrder(workflow)
	if err != nil {
		return e.failureResult(err), nil
	}
	var firstErr error
	for _, name := range order {
		job := workflow.Jobs[name]
		result, jobErr := e.executeJob(ctx, name, &job)
		e.Context.JobResults[name] = result
		if jobErr != nil && firstErr == nil {
			firstErr = jobErr
		}
	}
	if firstErr != nil {
		e.Context.Error = firstErr
		return e.failureResult(firstErr), nil
	}
	return e.successResult(), nil
}

func (e *Executor) initializeContext(request *models.Request) error {
	maps.Copy(e.Context.Inputs, request.Inputs)
	if request.Context != nil {
		if config, ok := request.Context["config"].(map[string]any); ok {
			if env, ok := config["env"].(map[string]any); ok {
				maps.Copy(e.Context.Env, env)
			}
		}
		if secrets, ok := request.Context["secrets"].(map[string]any); ok {
			for key, value := range secrets {
				if text, ok := value.(string); ok {
					e.Context.Secrets[key] = text
				}
			}
		}
		if cacheValue, ok := request.Context["cache"]; ok && cacheValue != nil {
			e.Context.Cache = decodeCache(cacheValue)
		}
		if vars, ok := request.Context["vars"].(map[string]any); ok {
			e.Context.Vars = vars
		}
	}
	if token := e.Context.Secrets["GITHUB_TOKEN"]; token != "" {
		if _, exists := e.Context.Env["GITHUB_TOKEN"]; !exists {
			e.Context.Env["GITHUB_TOKEN"] = token
		}
	}
	e.Context.Env["CI"] = "true"
	e.Context.Env["GITHUB_ACTIONS"] = "true"
	return nil
}

func jobOrder(workflow *models.Workflow) ([]string, error) {
	names := make([]string, 0, len(workflow.Jobs))
	for name := range workflow.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	visited := map[string]bool{}
	visiting := map[string]bool{}
	order := make([]string, 0, len(names))
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("circular needs involving job %s", name)
		}
		job, ok := workflow.Jobs[name]
		if !ok {
			return fmt.Errorf("job %s is not defined", name)
		}
		visiting[name] = true
		for _, need := range needsOf(job.Needs) {
			if err := visit(need); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		order = append(order, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func needsOf(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok && name != "" {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

func (e *Executor) executeJob(ctx context.Context, name string, job *models.WorkflowJob) (JobResult, error) {
	result := JobResult{Status: "success", Outputs: map[string]any{}}
	dependencyFailed := false
	for _, need := range needsOf(job.Needs) {
		dep, ok := e.Context.JobResults[need]
		if ok && (dep.Status == "failure" || dep.Status == "skipped") {
			dependencyFailed = true
		}
	}
	e.jobFailed = dependencyFailed
	if job.If != nil {
		guard, err := e.evaluateGuard(ctx, job.If)
		if err != nil {
			result.Status = "failure"
			result.Error = err.Error()
			return result, fmt.Errorf("job %s: %w", name, err)
		}
		if !guard.run {
			result.Status = "skipped"
			return result, nil
		}
	} else if dependencyFailed {
		result.Status = "skipped"
		return result, nil
	}
	jobCtx := ctx
	if job.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithTimeout(ctx, time.Duration(job.TimeoutMinutes)*time.Minute)
		defer cancel()
	}
	e.jobDeadline = time.Time{}
	if job.TimeoutMinutes > 0 {
		e.jobDeadline = time.Now().Add(time.Duration(job.TimeoutMinutes) * time.Minute)
	}
	envSnapshot := make(map[string]any, len(e.Context.Env))
	maps.Copy(envSnapshot, e.Context.Env)
	for key, value := range job.Env {
		evaluated, evalErr := e.evaluateExpression(ctx, fmt.Sprintf("%v", value))
		if evalErr != nil {
			maps.Copy(e.Context.Env, envSnapshot)
			result.Status = "failure"
			result.Error = evalErr.Error()
			return result, fmt.Errorf("job %s: %w", name, evalErr)
		}
		e.Context.Env[key] = evaluated
	}
	err := e.executeSteps(jobCtx, job)
	outputs := map[string]any{}
	for outputName, expression := range job.Outputs {
		value, evalErr := e.evaluateExpression(ctx, fmt.Sprintf("%v", expression))
		if evalErr != nil {
			outputs[outputName] = ""
			continue
		}
		outputs[outputName] = value
	}
	for key := range e.Context.Env {
		if _, existed := envSnapshot[key]; !existed {
			delete(e.Context.Env, key)
		}
	}
	maps.Copy(e.Context.Env, envSnapshot)
	result.Outputs = outputs
	if err != nil {
		result.Status = "failure"
		result.Error = err.Error()
		return result, fmt.Errorf("job %s: %w", name, err)
	}
	return result, nil
}

func (e *Executor) executeSteps(ctx context.Context, job *models.WorkflowJob) error {
	e.jobFailed = false
	e.Context.Error = nil
	var firstErr error
	for index, step := range job.Steps {
		if err := ctx.Err(); err != nil {
			e.jobFailed = true
			return err
		}
		stepName := fmt.Sprintf("step_%d", index+1)
		if step.ID != "" {
			stepName = step.ID
		} else if step.Name != "" {
			stepName = step.Name
		}
		guard, err := e.evaluateGuard(ctx, step.If)
		if err != nil {
			firstErr = fmt.Errorf("step %s: %w", stepName, err)
			e.jobFailed = true
			break
		}
		if e.jobFailed && !guard.always && !guard.failure {
			continue
		}
		if !guard.run {
			continue
		}
		if step.Shell != "" {
			e.Context.Shell = step.Shell
		}
		env, err := e.buildStepEnv(ctx, &step)
		if err != nil {
			firstErr = fmt.Errorf("step %s: %w", stepName, err)
			e.jobFailed = true
			break
		}
		e.emit(fmt.Sprintf("##[group]%s", stepName))
		var stepErr error
		if strings.TrimSpace(step.Run) != "" {
			stepErr = fmt.Errorf("this evaluator runs gha-lite policies net-only and refuses a run: step (job step %s)", stepName)
		} else if step.Uses != "" {
			stepErr = e.executeStepUses(ctx, &step, env)
		}
		e.emit("##[endgroup]")
		if stepErr != nil {
			e.emit(fmt.Sprintf("##[error]step %s failed: %v", stepName, stepErr))
			if step.ContinueOnError {
				continue
			}
			e.jobFailed = true
			e.Context.Error = stepErr
			if firstErr == nil {
				firstErr = fmt.Errorf("step %s: %w", stepName, stepErr)
			}
			continue
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if !e.jobDeadline.IsZero() && time.Now().After(e.jobDeadline) {
		e.jobFailed = true
		return fmt.Errorf("job exceeded timeout-minutes")
	}
	return nil
}

func (e *Executor) emit(line string) {
	if e.opts.MaxConsoleBytes > 0 {
		if e.Context.console >= e.opts.MaxConsoleBytes {
			return
		}
		remaining := e.opts.MaxConsoleBytes - e.Context.console
		if int64(len(line)) > remaining {
			line = line[:remaining]
		}
	}
	e.Context.Console = append(e.Context.Console, line)
	e.Context.console += int64(len(line)) + 1
}

func (e *Executor) successResult() *models.Result {
	outputs := models.Outputs{}
	for name, result := range e.Context.JobResults {
		outputs[name] = result.Outputs
	}
	if used, limit := e.outputBytes(outputs); limit > 0 && used > limit {
		err := fmt.Errorf("workflow outputs exceed the configured limit")
		e.emit("##[error]" + err.Error())
		return e.failureResult(err)
	}
	return &models.Result{
		Status:        models.ExecutionComplete,
		ExitStatus:    models.ExitSuccess,
		Outputs:       outputs,
		Cache:         e.Context.Cache,
		Annotations:   e.annotationMap(),
		ConsoleOutput: strings.Join(e.Context.Console, "\n"),
	}
}

func (e *Executor) failureResult(err error) *models.Result {
	annotations := e.annotationMap()
	errors := []models.Annotation{}
	if err != nil {
		errors = append(errors, models.Annotation{AnnotationLevel: models.AnnotationError, Message: err.Error(), Title: err.Error()})
	}
	annotations["error"] = errors
	return &models.Result{
		Status:        models.ExecutionComplete,
		ExitStatus:    models.ExitFailure,
		Outputs:       models.Outputs{},
		Cache:         e.Context.Cache,
		Annotations:   annotations,
		ConsoleOutput: strings.Join(e.Context.Console, "\n"),
	}
}

func (e *Executor) annotationMap() models.Annotations {
	out := models.Annotations{}
	for level, list := range e.Context.Annotations {
		copied := make([]models.Annotation, len(list))
		copy(copied, list)
		out[level] = copied
	}
	return out
}

func (e *Executor) outputBytes(outputs models.Outputs) (int64, int64) {
	if e.opts.MaxOutputBytes <= 0 {
		return 0, e.opts.MaxOutputBytes
	}
	data, err := json.Marshal(outputs)
	if err != nil {
		return 0, e.opts.MaxOutputBytes
	}
	return int64(len(data)), e.opts.MaxOutputBytes
}

func decodeCache(value any) models.Cache {
	data, err := json.Marshal(value)
	if err != nil {
		return models.Cache{}
	}
	var out models.Cache
	if err := json.Unmarshal(data, &out); err != nil {
		return models.Cache{}
	}
	if out == nil {
		out = models.Cache{}
	}
	return out
}

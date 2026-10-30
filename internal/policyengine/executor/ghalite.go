package executor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/workflow"
)

const defaultGhaLiteTimeout = 30 * time.Second

type GhaLiteOptions struct {
	Sandbox         sandbox.Runner
	ResolveAction   workflow.ActionResolver
	Timeout         time.Duration
	MaxConsoleBytes int64
	MaxOutputBytes  int64
}

type GhaLite struct {
	sandbox         sandbox.Runner
	resolveAction   workflow.ActionResolver
	timeout         time.Duration
	maxConsoleBytes int64
	maxOutputBytes  int64
}

func NewGhaLite(opts GhaLiteOptions) *GhaLite {
	resolver := opts.ResolveAction
	if resolver == nil {
		resolver = DefaultActionResolver()
	}
	timeout := opts.Timeout
	timeout = opt.Positive(timeout, defaultGhaLiteTimeout)
	return &GhaLite{
		sandbox:         opts.Sandbox,
		resolveAction:   resolver,
		timeout:         timeout,
		maxConsoleBytes: opts.MaxConsoleBytes,
		maxOutputBytes:  opts.MaxOutputBytes,
	}
}

func (g *GhaLite) Kind() string {
	return PolicyGhaLiteNSID
}

func (g *GhaLite) Execute(ctx context.Context, record PolicyRecord, eval *EvalContext, permissions map[string]any) (Verdict, error) {
	workflowText, err := recordWorkflow(record)
	if err != nil {
		return Deny(err.Error(), PolicyGhaLiteNSID), nil
	}
	if refusal := runStepRefusal(workflowText); refusal != "" {
		return Deny(refusal, PolicyGhaLiteNSID), nil
	}
	var snapshot *trust.Snapshot
	if eval.Host != nil {
		selfService, counterpartyService := snapshotServices(eval.Perspective)
		built, snapshotErr := trust.BuildSnapshotFor(ctx, eval.Host, eval.SelfDid, eval.CounterpartyDid, selfService, counterpartyService)
		if snapshotErr != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			return Deny("policy trust resolution failed: "+snapshotErr.Error(), PolicyGhaLiteNSID), nil
		}
		snapshot = &built
	}
	permissions = effectivePermissions(record, permissions)
	request := buildFullRequest(workflowText, eval, permissions, snapshot)
	result, runErr := g.run(ctx, request)
	if runErr != nil {
		return Verdict{}, runErr
	}
	if result == nil {
		return Deny("policy workflow produced no result", PolicyGhaLiteNSID), nil
	}
	return statusToVerdict(result), nil
}

func (g *GhaLite) Scope(ctx context.Context, record PolicyRecord, input ScopeInput) (Verdict, bool, error) {
	workflowText, err := recordWorkflow(record)
	if err != nil {
		return Deny(err.Error(), PolicyGhaLiteNSID), true, nil
	}
	if refusal := runStepRefusal(workflowText); refusal != "" {
		return Deny(refusal, PolicyGhaLiteNSID), true, nil
	}
	var host trust.Host
	if input.ResolveOperatorDid != nil || input.Vouches != nil {
		host = scopeHost{
			resolveOperatorDid:    input.ResolveOperatorDid,
			resolveOperatorDidFor: input.ResolveOperatorDidFor,
			vouches:               input.Vouches,
		}
	}
	var snapshot *trust.Snapshot
	if host != nil {
		selfService, counterpartyService := snapshotServices(input.Perspective)
		built, snapshotErr := trust.BuildSnapshotFor(ctx, host, input.SelfDid, input.CounterpartyDid, selfService, counterpartyService)
		if snapshotErr != nil {
			if ctx.Err() != nil {
				return Verdict{}, false, ctx.Err()
			}
			return Deny("policy trust resolution failed: "+snapshotErr.Error(), PolicyGhaLiteNSID), true, nil
		}
		snapshot = &built
	}
	request := buildScopeRequest(workflowText, record, input, permissionsFromValue(record.Value), snapshot)
	result, runErr := g.run(ctx, request)
	if runErr != nil {
		return Verdict{}, false, runErr
	}
	if result == nil {
		return Deny("policy workflow produced no result", PolicyGhaLiteNSID), true, nil
	}
	return statusToVerdict(result), true, nil
}

func (g *GhaLite) run(ctx context.Context, request *models.Request) (*models.Result, error) {
	runCtx := ctx
	cancel := func() {}
	if g.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, g.timeout)
	}
	defer cancel()
	executor := workflow.New(workflow.Options{
		Sandbox:         g.sandbox,
		ResolveAction:   g.resolveAction,
		MaxConsoleBytes: g.maxConsoleBytes,
		MaxOutputBytes:  g.maxOutputBytes,
	})
	result, err := executor.Execute(runCtx, request)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return &models.Result{
			Status:     models.ExecutionComplete,
			ExitStatus: models.ExitFailure,
			Annotations: models.Annotations{
				"error": []models.Annotation{{
					AnnotationLevel: models.AnnotationError,
					Message:         fmt.Sprintf("policy workflow timed out after %s", g.timeout),
					Title:           "policy workflow timed out",
				}},
			},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordWorkflow(record PolicyRecord) (string, error) {
	if record.Value == nil {
		return "", fmt.Errorf("gha-lite record has no workflow")
	}
	value, ok := record.Value["workflow"].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("gha-lite record has no workflow")
	}
	return value, nil
}

func runStepRefusal(workflowText string) string {
	raw, err := json.Marshal(workflowText)
	if err != nil {
		return ""
	}
	if job, ok := workflow.HasRunStep(raw); ok {
		return fmt.Sprintf("%s a run: step (job %s)", NetOnlyRefused, job)
	}
	return ""
}

func effectivePermissions(record PolicyRecord, explicit map[string]any) map[string]any {
	if explicit != nil {
		return explicit
	}
	return permissionsFromValue(record.Value)
}

func permissionsFromValue(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	raw, ok := value["permissions"]
	if !ok {
		return nil
	}
	if permissions, ok := raw.(map[string]any); ok {
		return permissions
	}
	return map[string]any{"value": raw}
}

func buildFullRequest(workflowText string, eval *EvalContext, permissions map[string]any, snapshot *trust.Snapshot) *models.Request {
	args := eval.Args
	if args == nil {
		args = map[string]any{}
	}
	argsJSON, _ := json.Marshal(args)
	inputs := map[string]any{
		"self-did":           eval.SelfDid,
		"subject-did":        eval.SubjectDid,
		"root-requester-did": eval.RootRequesterDid,
		"counterparty-did":   eval.CounterpartyDid,
		"perspective":        string(eval.Perspective),
		"policy-args":        string(argsJSON),
	}
	if permissions != nil {
		permissionsJSON, err := json.Marshal(permissions)
		if err == nil {
			inputs["permissions"] = string(permissionsJSON)
		}
	}
	if record := recordInput(eval.RFP); record != "" {
		inputs["rfp"] = record
	}
	if record := recordInput(eval.Bid); record != "" {
		inputs["bid"] = record
	}
	if record := recordInput(eval.Accept); record != "" {
		inputs["accept"] = record
	}
	context := requestContext(eval, snapshot, permissions)
	return &models.Request{
		Inputs:   inputs,
		Workflow: mustJSONString(workflowText),
		Context:  context,
		Stack:    eval.Stack,
	}
}

func buildScopeRequest(workflowText string, record PolicyRecord, input ScopeInput, permissions map[string]any, snapshot *trust.Snapshot) *models.Request {
	args := input.Args
	if args == nil {
		args = map[string]any{}
	}
	argsJSON, _ := json.Marshal(args)
	inputs := map[string]any{
		"mode":             "scope",
		"self-did":         input.SelfDid,
		"counterparty-did": input.CounterpartyDid,
		"perspective":      string(input.Perspective),
		"policy-args":      string(argsJSON),
	}
	if permissions != nil {
		permissionsJSON, err := json.Marshal(permissions)
		if err == nil {
			inputs["permissions"] = string(permissionsJSON)
		}
	}
	ref := record.Ref()
	eval := &EvalContext{
		PolicyName:       policyName(record),
		Args:             args,
		Perspective:      input.Perspective,
		SelfDid:          input.SelfDid,
		SubjectDid:       input.CounterpartyDid,
		RootRequesterDid: rootRequester(input.Perspective, input.SelfDid, input.CounterpartyDid),
		CounterpartyDid:  input.CounterpartyDid,
		PolicyRef:        &ref,
	}
	return &models.Request{
		Inputs:   inputs,
		Workflow: mustJSONString(workflowText),
		Context:  requestContext(eval, snapshot, permissions),
	}
}

func requestContext(eval *EvalContext, snapshot *trust.Snapshot, permissions map[string]any) map[string]any {
	repository := atproto.RepoFromURI(eval.PolicyRefURI())
	repository = cmp.Or(repository, eval.SelfDid)
	context := map[string]any{
		"config": map[string]any{
			"env": map[string]any{
				"GITHUB_REPOSITORY": repository,
				"GITHUB_ACTOR":      eval.SelfDid,
			},
		},
	}
	if permissions != nil {
		context["permissions"] = permissions
	}
	cache := make(models.Cache, len(eval.Cache)+1)
	maps.Copy(cache, eval.Cache)
	if snapshot != nil {
		cache["trust/"+eval.CounterpartyDid] = snapshot.CacheFiles()
	}
	context["cache"] = cache
	return context
}

func (e *EvalContext) PolicyRefURI() string {
	if e.PolicyRef == nil {
		return ""
	}
	return e.PolicyRef.URI
}

func policyName(record PolicyRecord) string {
	if record.Value != nil {
		if name, ok := record.Value["name"].(string); ok && name != "" {
			return name
		}
	}
	return "policy"
}

func rootRequester(perspective Perspective, selfDid, counterpartyDid string) string {
	if perspective == PerspectiveRequester {
		return selfDid
	}
	return counterpartyDid
}

func recordInput(record *PolicyRecord) string {
	if record == nil || record.URI == "" {
		return ""
	}
	value := map[string]any{"uri": record.URI, "cid": record.CID}
	if record.Value != nil {
		value["value"] = record.Value
	}
	if record.Payload != nil {
		value["payload"] = record.Payload
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func mustJSONString(value string) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

type scopeHost struct {
	resolveOperatorDid    func(context.Context, string) (string, bool, error)
	resolveOperatorDidFor func(context.Context, string, string) (string, bool, error)
	vouches               func(context.Context, string, string) (bool, error)
}

func (h scopeHost) Resolve(ctx context.Context, ref atproto.StrongRef) (map[string]any, error) {
	return nil, fmt.Errorf("record resolution is not available in scope mode")
}

func (h scopeHost) ResolveOperatorDid(ctx context.Context, did string) (string, bool, error) {
	if h.resolveOperatorDid == nil {
		return "", false, nil
	}
	return h.resolveOperatorDid(ctx, did)
}

func (h scopeHost) ResolveOperatorDidFor(ctx context.Context, did, service string) (string, bool, error) {
	if h.resolveOperatorDidFor == nil {
		return h.ResolveOperatorDid(ctx, did)
	}
	return h.resolveOperatorDidFor(ctx, did, service)
}

func snapshotServices(perspective Perspective) (string, string) {
	if perspective == PerspectiveRequester {
		return trust.RequesterAssociate, trust.BidderAssociate
	}
	return trust.BidderAssociate, trust.RequesterAssociate
}

func (h scopeHost) Vouches(ctx context.Context, voucher, subject string) (bool, error) {
	if h.vouches == nil {
		return false, nil
	}
	return h.vouches(ctx, voucher, subject)
}

func (h scopeHost) Log(level, msg string, meta map[string]any) {}

func statusToVerdict(result *models.Result) Verdict {
	if result == nil {
		return Deny("policy workflow produced no result", PolicyGhaLiteNSID)
	}
	if result.Status == models.ExecutionComplete && result.ExitStatus == models.ExitSuccess {
		return verdictOfOutputs(result.Outputs)
	}
	message := describeStatus(result)
	if strings.HasPrefix(message, ExpressionDenied) {
		return Deny(message, SandboxDeniedPolicyID)
	}
	return Deny(message, PolicyGhaLiteNSID)
}

func describeStatus(result *models.Result) string {
	if result == nil {
		return "policy workflow produced no result"
	}
	if result.Annotations != nil {
		if raw, ok := result.Annotations["error"]; ok {
			if message := firstAnnotationMessage(raw); message != "" {
				return message
			}
		}
	}
	if result.ExitStatus == models.ExitFailure {
		return "policy workflow exited with failure"
	}
	if result.Status != "" {
		return "policy engine status: " + string(result.Status)
	}
	return "policy engine status: unknown"
}

func firstAnnotationMessage(raw any) string {
	switch typed := raw.(type) {
	case string:
		return typed
	case []string:
		if len(typed) > 0 {
			return typed[0]
		}
	case []models.Annotation:
		if len(typed) > 0 {
			return typed[0].Message
		}
	case []any:
		if len(typed) == 0 {
			return ""
		}
		switch first := typed[0].(type) {
		case string:
			return first
		case models.Annotation:
			return first.Message
		case map[string]any:
			if message, ok := first["message"].(string); ok {
				return message
			}
			if message, ok := first["msg"].(string); ok {
				return message
			}
		}
	}
	return ""
}

func verdictOfOutputs(outputs models.Outputs) Verdict {
	allowed := true
	violations := []Violation{}
	for _, raw := range outputs {
		entry, ok := asMap(raw)
		if !ok {
			continue
		}
		allowValue, exists := entry["allow"]
		if !exists {
			continue
		}
		if !isTrue(allowValue) {
			allowed = false
		}
		violations = append(violations, parseViolations(entry["violations"])...)
	}
	if allowed {
		return Allow()
	}
	if len(violations) == 0 {
		violations = append(violations, Violation{Msg: "the policy action answered allow=false", PolicyID: PolicyGhaLiteNSID})
	}
	return Verdict{Allow: false, Violations: violations}
}

func asMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[key] = value
		}
		return out, true
	default:
		return nil, false
	}
}

func isTrue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed == "true"
	default:
		return fmt.Sprintf("%v", typed) == "true"
	}
}

func parseViolations(value any) []Violation {
	if value == nil {
		return nil
	}
	raw := value
	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return []Violation{{Msg: text, PolicyID: PolicyGhaLiteNSID}}
		}
		raw = decoded
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	violations := make([]Violation, 0, len(list))
	for _, item := range list {
		switch typed := item.(type) {
		case string:
			violations = append(violations, Violation{Msg: typed, PolicyID: PolicyGhaLiteNSID})
		case map[string]any:
			message, _ := typed["msg"].(string)
			if message == "" {
				message = fmt.Sprintf("%v", typed)
			}
			policyID := any(PolicyGhaLiteNSID)
			if value, ok := typed["policyId"]; ok {
				policyID = value
			}
			violations = append(violations, Violation{Msg: message, PolicyID: policyID})
		default:
			violations = append(violations, Violation{Msg: fmt.Sprintf("%v", typed), PolicyID: PolicyGhaLiteNSID})
		}
	}
	return violations
}

package bidder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

const (
	PolicyPerspectiveBidder = "bidder"

	PolicyModeScope = "scope"

	PolicyModeFull = "full"

	PolicyRunTTLSeconds = int64(3600)
)

type PolicyStrongRef struct {
	URI string

	CID string
}

func (r *PolicyStrongRef) api() *policyv1alpha1.PolicyStrongRef {
	if r == nil || r.URI == "" {
		return nil
	}
	return &policyv1alpha1.PolicyStrongRef{URI: r.URI, CID: r.CID}
}

type PolicyInput struct {
	Perspective string

	SelfDid string

	SubjectDid string

	CounterpartyDid string

	RootRequesterDid string

	RFP *PolicyStrongRef

	Bid *PolicyStrongRef

	Accept *PolicyStrongRef

	Mode string
}

type PolicyDecision struct {
	Allow bool

	Violations []string

	Reason string

	Run string
}

type PolicyRunClient interface {
	CreatePolicyWorkflowRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) error

	WaitPolicyWorkflowRun(ctx context.Context, target ref.Ref) (*policyv1alpha1.PolicyWorkflowRun, error)
}

type KCPPolicyRuns struct {
	Res *clusterResource[policyv1alpha1.PolicyWorkflowRun]

	Cluster string

	Poll time.Duration

	Log *slog.Logger
}

func NewKCPPolicyRuns(store *kcpstore.Store, cluster string) *KCPPolicyRuns {
	return &KCPPolicyRuns{Res: newClusterResource[policyv1alpha1.PolicyWorkflowRun](store, PolicyWorkflowRunGVR), Cluster: cluster}
}

func (c *KCPPolicyRuns) CreatePolicyWorkflowRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) error {
	if c == nil || c.Res == nil {
		return fmt.Errorf("bidder: the policy run client is not configured")
	}
	run.TypeMeta = metav1.TypeMeta{APIVersion: policyv1alpha1.GroupVersion.String(), Kind: "PolicyWorkflowRun"}
	return c.Res.Create(ctx, c.Cluster, run)
}

func (c *KCPPolicyRuns) WaitPolicyWorkflowRun(ctx context.Context, target ref.Ref) (*policyv1alpha1.PolicyWorkflowRun, error) {
	if c == nil || c.Res == nil {
		return nil, fmt.Errorf("bidder: the policy run client is not configured")
	}
	poll := c.Poll
	if poll <= 0 {
		poll = DefaultPolicyPoll
	}
	for {
		run, err := c.Res.Get(ctx, target.LogicalCluster, target.Name)
		if err == nil && run != nil && policyRunTerminal(run.Status.Phase) {
			return run, nil
		}
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			c.cancel(target)
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *KCPPolicyRuns) cancel(target ref.Ref) {
	if c == nil || c.Res == nil {
		return
	}
	patch, err := json.Marshal([]map[string]any{{"op": "add", "path": "/spec/cancel", "value": true}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Res.PatchJSON(ctx, target.LogicalCluster, target.Name, patch); err != nil && c.Log != nil {
		c.Log.Warn("cancelling an abandoned policy run failed", "name", target.Name, "err", err)
	}
}

func policyRunTerminal(phase policyv1alpha1.PolicyWorkflowRunPhase) bool {
	switch phase {
	case policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed, policyv1alpha1.PolicyWorkflowRunCancelled:
		return true
	default:
		return false
	}
}

type PolicyRecordResolver interface {
	Resolve(ctx context.Context, ref market.RecordRef) (market.ResolvedRecord, error)
}

type PolicyEvaluator struct {
	Runs PolicyRunClient

	Cluster string

	EngineRef string

	ScopePolicy string

	Args map[string]string

	Timeout time.Duration

	Poll time.Duration

	Records PolicyRecordResolver

	Log *slog.Logger

	Now func() time.Time

	counter atomic.Uint64
}

func (e *PolicyEvaluator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *PolicyEvaluator) Scope(ctx context.Context, input PolicyInput) PolicyDecision {
	if e == nil || e.ScopePolicy == "" {
		return PolicyDecision{Allow: true}
	}
	input.Perspective = PolicyPerspectiveBidder
	input.Mode = PolicyModeScope
	return e.run(ctx, PolicyRunRequest{WorkflowRef: e.ScopePolicy, Input: input})
}

func (e *PolicyEvaluator) EvaluatePolicies(ctx context.Context, refs []market.StrongRef, input PolicyInput) PolicyDecision {
	if len(refs) == 0 {
		return PolicyDecision{Allow: true}
	}
	if input.Perspective == "" {
		input.Perspective = PolicyPerspectiveBidder
	}
	if input.Mode == "" {
		input.Mode = PolicyModeFull
	}
	for _, entry := range refs {
		refCopy := entry
		record, err := e.resolvePolicy(ctx, refCopy)
		if err != nil {
			return PolicyDecision{Reason: fmt.Sprintf("failed to resolve policy ref %s: %v", entry.URI, err)}
		}
		workflow, err := workflowOfRecord(record.Value)
		if err != nil {
			return PolicyDecision{Reason: fmt.Sprintf("policy ref %s: %v", entry.URI, err)}
		}
		decision := e.run(ctx, PolicyRunRequest{Workflow: workflow, Input: input})
		if !decision.Allow {
			return decision
		}
	}
	return PolicyDecision{Allow: true}
}

func (e *PolicyEvaluator) resolvePolicy(ctx context.Context, entry market.StrongRef) (market.ResolvedRecord, error) {
	if e == nil || e.Records == nil {
		return market.ResolvedRecord{}, fmt.Errorf("the bidder has no record resolver")
	}
	if _, err := market.ParseRecordRef(entry.URI, entry.CID); err != nil {
		return market.ResolvedRecord{}, err
	}
	return e.Records.Resolve(ctx, market.RecordRef{URI: entry.URI, CID: entry.CID})
}

const PolicyRecordType = "computer.socialweb.temp.policy.ghalite"

func workflowOfRecord(value map[string]any) (json.RawMessage, error) {
	if kind, _ := value["$type"].(string); kind != PolicyRecordType {
		return nil, fmt.Errorf("the policy record type %q is not gha-lite and is denied", kind)
	}
	raw, ok := value["workflow"]
	if !ok || raw == nil {
		return nil, fmt.Errorf("the policy record names no workflow")
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("the policy record names an empty workflow")
		}
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(v), &parsed); err != nil || parsed == nil {
			return nil, fmt.Errorf("the policy record's workflow is not a gha-lite workflow mapping")
		}
		return json.Marshal(parsed)
	case map[string]any:
		return json.Marshal(v)
	default:
		return nil, fmt.Errorf("the policy record's workflow is not a string or object")
	}
}

type PolicyRunRequest struct {
	WorkflowRef string

	Workflow json.RawMessage

	Input PolicyInput

	Timeout time.Duration
}

func (e *PolicyEvaluator) run(ctx context.Context, req PolicyRunRequest) PolicyDecision {
	if e == nil || e.Runs == nil {
		return PolicyDecision{Reason: "the policy run client is not configured"}
	}
	if e.EngineRef == "" {
		return PolicyDecision{Reason: "no PolicyEngine is configured for this bidder"}
	}
	if req.WorkflowRef == "" && len(req.Workflow) == 0 {
		return PolicyDecision{Reason: "the policy run names no workflow"}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = e.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultPolicyTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := e.buildRun(req)
	if err := e.Runs.CreatePolicyWorkflowRun(runCtx, run); err != nil {
		return PolicyDecision{Reason: "the policy run could not be created: " + err.Error()}
	}
	target := ref.New(e.Cluster, "", run.Name)
	finished, err := e.Runs.WaitPolicyWorkflowRun(runCtx, target)
	if err != nil {
		return PolicyDecision{Reason: "the policy run did not finish: " + err.Error(), Run: run.Name}
	}
	return decisionOfRun(finished)
}

func (e *PolicyEvaluator) buildRun(req PolicyRunRequest) *policyv1alpha1.PolicyWorkflowRun {
	input := req.Input
	if input.Perspective == "" {
		input.Perspective = PolicyPerspectiveBidder
	}
	if input.Mode == "" {
		input.Mode = PolicyModeFull
	}
	inputs := map[string]string{"mode": input.Mode}
	if input.SelfDid != "" {
		inputs["self-did"] = input.SelfDid
	}
	if input.SubjectDid != "" {
		inputs["subject-did"] = input.SubjectDid
	}
	if input.CounterpartyDid != "" {
		inputs["counterparty-did"] = input.CounterpartyDid
	}
	if input.RootRequesterDid != "" {
		inputs["root-requester-did"] = input.RootRequesterDid
	}
	args := map[string]string{}
	for key, value := range e.Args {
		args[key] = value
	}
	inputs["policy-args"] = marshalArgs(args)
	deadline := int64(e.Timeout / time.Second)
	if req.Timeout > 0 {
		deadline = int64(req.Timeout / time.Second)
	}
	if deadline <= 0 {
		deadline = int64(DefaultPolicyTimeout / time.Second)
	}
	ttl := PolicyRunTTLSeconds
	run := &policyv1alpha1.PolicyWorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: e.runName()},
		Spec: policyv1alpha1.PolicyWorkflowRunSpec{
			EngineRef:               e.EngineRef,
			WorkflowRef:             req.WorkflowRef,
			Workflow:                append(json.RawMessage(nil), req.Workflow...),
			Inputs:                  inputs,
			Args:                    args,
			Perspective:             input.Perspective,
			SelfDid:                 input.SelfDid,
			SubjectDid:              input.SubjectDid,
			CounterpartyDid:         input.CounterpartyDid,
			RootRequesterDid:        input.RootRequesterDid,
			RFP:                     input.RFP.api(),
			Bid:                     input.Bid.api(),
			Accept:                  input.Accept.api(),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
		},
	}
	return run
}

func (e *PolicyEvaluator) runName() string {
	now := e.now().UnixNano()
	count := e.counter.Add(1)
	return fmt.Sprintf("policy-%d-%d", now, count)
}

func marshalArgs(args map[string]string) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func decisionOfRun(run *policyv1alpha1.PolicyWorkflowRun) PolicyDecision {
	if run == nil {
		return PolicyDecision{Reason: "the policy run returned no status"}
	}
	name := run.Name
	switch run.Status.Phase {
	case policyv1alpha1.PolicyWorkflowRunSucceeded:
	case policyv1alpha1.PolicyWorkflowRunFailed:
		return PolicyDecision{Reason: policyRunFailure(run), Run: name}
	case policyv1alpha1.PolicyWorkflowRunCancelled:
		return PolicyDecision{Reason: "the policy run was cancelled", Run: name}
	default:
		return PolicyDecision{Reason: fmt.Sprintf("the policy run ended in phase %q", run.Status.Phase), Run: name}
	}
	if run.Status.ExitStatus != "" && run.Status.ExitStatus != policyv1alpha1.PolicyExitStatusSuccess {
		return PolicyDecision{Reason: "the policy workflow failed", Run: name}
	}
	allow, ok := outputBool(run.Status.Outputs, "allow")
	if !ok {
		return PolicyDecision{Reason: "the policy run produced no allow output", Run: name}
	}
	violations := outputStrings(run.Status.Outputs, "violations")
	if !allow {
		reason := "policy rejected"
		if len(violations) > 0 {
			reason += ": " + strings.Join(violations, ", ")
		}
		return PolicyDecision{Violations: violations, Reason: reason, Run: name}
	}
	return PolicyDecision{Allow: true, Violations: violations, Run: name}
}

func policyRunFailure(run *policyv1alpha1.PolicyWorkflowRun) string {
	for _, condition := range run.Status.Conditions {
		if condition.Status == metav1.ConditionTrue && condition.Message != "" {
			return condition.Message
		}
	}
	if raw, ok := run.Status.Annotations["error"]; ok && len(raw) > 0 {
		if message := policyAnnotationMessage(raw); message != "" {
			return message
		}
		return strings.TrimSpace(string(raw))
	}
	return "the policy run failed"
}

func policyAnnotationMessage(raw json.RawMessage) string {
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	items := []map[string]any{}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Items) > 0 {
		items = envelope.Items
	} else if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	for _, item := range items {
		for _, key := range []string{"message", "title", "msg"} {
			if text, ok := item[key].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}

func outputBool(outputs map[string]json.RawMessage, key string) (bool, bool) {
	values := outputValues(outputs, key)
	found := false
	allow := true
	for _, raw := range values {
		value, ok := outputBoolValue(raw)
		if !ok {
			continue
		}
		found = true
		if !value {
			allow = false
		}
	}
	if !found {
		return false, false
	}
	return allow, true
}

func outputBoolValue(raw json.RawMessage) (bool, bool) {
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1", "yes", "allow", "allowed", "success":
			return true, true
		case "false", "0", "no", "deny", "denied", "failure":
			return false, true
		default:
			return false, false
		}
	}
	var object struct {
		Allow *bool `json:"allow"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Allow != nil {
		return *object.Allow, true
	}
	return false, false
}

func outputStrings(outputs map[string]json.RawMessage, key string) []string {
	var out []string
	for _, raw := range outputValues(outputs, key) {
		out = append(out, outputStringValues(raw)...)
	}
	return out
}

func outputStringValues(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		var nested []string
		if json.Unmarshal([]byte(text), &nested) == nil {
			return nested
		}
		parts := strings.Split(text, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return nil
}

func outputValues(outputs map[string]json.RawMessage, key string) []json.RawMessage {
	values := make([]json.RawMessage, 0, len(outputs))
	if raw, ok := outputs[key]; ok && len(raw) > 0 {
		values = append(values, raw)
	}
	for name, raw := range outputs {
		if name == key || len(raw) == 0 {
			continue
		}
		var job map[string]json.RawMessage
		if json.Unmarshal(raw, &job) != nil {
			continue
		}
		if value, ok := job[key]; ok && len(value) > 0 {
			values = append(values, value)
		}
	}
	return values
}

package requester

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/policyrun"
	bundledactions "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	PolicyPerspectiveRequester = "requester"

	PolicyModeScope = "scope"

	PolicyModeFull = "full"
)

var PolicyWorkflowRunGVR = policyv1alpha1.PolicyWorkflowRunGVR

type PolicyStrongRef struct {
	URI string

	CID string
}

func (r *PolicyStrongRef) api() *commonv1.StrongRef {
	if r == nil || r.URI == "" {
		return nil
	}
	return &commonv1.StrongRef{URI: r.URI, CID: r.CID}
}

func (r PolicyStrongRef) market() market.RecordRef {
	return market.RecordRef{URI: r.URI, CID: r.CID}
}

type PolicyInput struct {
	Workflow string

	SelfDid string

	SubjectDid string

	CounterpartyDid string

	RootRequesterDid string

	RFP *PolicyStrongRef

	Bid *PolicyStrongRef

	Accept *PolicyStrongRef

	Mode string
}

type PolicyDecision = policyrun.Decision

type PolicyRunStore interface {
	CreatePolicyWorkflowRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) error

	WaitPolicyWorkflowRun(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflowRun, error)
}

type KCPPolicyRuns struct {
	Res *kcpstore.Resource[policyv1alpha1.PolicyWorkflowRun]

	Cluster string

	Poll time.Duration

	Log *slog.Logger
}

func NewKCPPolicyRuns(res *kcpstore.Resource[policyv1alpha1.PolicyWorkflowRun], cluster, _ string) *KCPPolicyRuns {
	return &KCPPolicyRuns{Res: res.ClusterScoped(), Cluster: cluster}
}

func (c *KCPPolicyRuns) CreatePolicyWorkflowRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) error {
	if c == nil || c.Res == nil {
		return errors.New("requester: the policy run client is not configured")
	}
	run.TypeMeta = metav1.TypeMeta{APIVersion: policyv1alpha1.GroupVersion.String(), Kind: "PolicyWorkflowRun"}
	return c.Res.Create(ctx, c.Cluster, run)
}

func (c *KCPPolicyRuns) WaitPolicyWorkflowRun(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflowRun, error) {
	if c == nil || c.Res == nil {
		return nil, errors.New("requester: the policy run client is not configured")
	}
	poll := c.Poll
	poll = opt.Positive(poll, DefaultPolicyPoll)
	target := ref.New(c.Cluster, "", name)
	for {
		run, err := c.Res.Get(ctx, target)
		if err == nil && run != nil && policyrun.Terminal(run.Status.Phase) {
			return run, nil
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
	if err := c.Res.PatchJSON(ctx, target, patch); err != nil && c.Log != nil {
		c.Log.Warn("requester_policy_cancel_failed", "name", target.Name, "err", err)
	}
}

type PolicyEvaluator struct {
	Runs PolicyRunStore

	Namespace string

	EngineRef string

	Workflow string

	Args map[string]string

	Timeout time.Duration

	Poll time.Duration

	DID string

	Log *slog.Logger

	Now func() time.Time

	counter atomic.Uint64
}

func (e *PolicyEvaluator) log() *slog.Logger {
	if e == nil || e.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return e.Log
}

func (e *PolicyEvaluator) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *PolicyEvaluator) Scope(ctx context.Context, input PolicyInput) PolicyDecision {
	if e == nil || (e.Workflow == "" && input.Workflow == "") {
		return PolicyDecision{Allow: true}
	}
	input.Mode = PolicyModeScope
	return e.run(ctx, input)
}

func (e *PolicyEvaluator) Evaluate(ctx context.Context, input PolicyInput) PolicyDecision {
	if e == nil || (e.Workflow == "" && input.Workflow == "") {
		return PolicyDecision{Allow: true}
	}
	input.Mode = cmp.Or(input.Mode, PolicyModeFull)
	return e.run(ctx, input)
}

func (e *PolicyEvaluator) run(ctx context.Context, input PolicyInput) PolicyDecision {
	if e == nil || e.Runs == nil {
		return PolicyDecision{Reason: "the policy run client is not configured"}
	}
	if e.EngineRef == "" {
		return PolicyDecision{Reason: "no PolicyEngine is configured for this requester"}
	}
	workflow := input.Workflow
	workflow = cmp.Or(workflow, e.Workflow)
	if workflow == "" {
		return PolicyDecision{Reason: "the requester names no policy workflow"}
	}
	timeout := e.Timeout
	timeout = opt.Positive(timeout, DefaultPolicyTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run, err := e.buildRun(input, workflow)
	if err != nil {
		return PolicyDecision{Reason: err.Error()}
	}
	if err := e.Runs.CreatePolicyWorkflowRun(runCtx, run); err != nil {
		return PolicyDecision{Reason: "the policy run could not be created: " + err.Error()}
	}
	finished, err := e.Runs.WaitPolicyWorkflowRun(runCtx, run.Name)
	if err != nil {
		return PolicyDecision{Reason: "the policy run did not finish: " + err.Error(), Run: run.Name}
	}
	return policyrun.Decide(finished, outputBoolValue, outputStringValues)
}

func (e *PolicyEvaluator) buildRun(input PolicyInput, workflow string) (*policyv1alpha1.PolicyWorkflowRun, error) {
	if e.DID == "" {
		return nil, errors.New("the requester has no DID for the policy run")
	}
	input.Mode = cmp.Or(input.Mode, PolicyModeFull)
	self := input.SelfDid
	self = cmp.Or(self, e.DID)
	counterparty := input.CounterpartyDid
	counterparty = cmp.Or(counterparty, input.SubjectDid)
	inputs := map[string]string{"mode": input.Mode, "self-did": self, "perspective": PolicyPerspectiveRequester}
	if input.SubjectDid != "" {
		inputs["subject-did"] = input.SubjectDid
	}
	if counterparty != "" {
		inputs["counterparty-did"] = counterparty
	}
	if input.RootRequesterDid != "" {
		inputs["root-requester-did"] = input.RootRequesterDid
	}
	args := map[string]string{}
	maps.Copy(args, e.Args)
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	inputs["policy-args"] = string(rawArgs)
	deadline := int64(e.Timeout / time.Second)
	if deadline <= 0 {
		deadline = int64(DefaultPolicyTimeout / time.Second)
	}
	ttl := PolicyRunTTLSeconds
	return &policyv1alpha1.PolicyWorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: e.runName(),
		},
		Spec: policyv1alpha1.PolicyWorkflowRunSpec{
			EngineRef:               e.EngineRef,
			WorkflowRef:             workflow,
			Inputs:                  inputs,
			Args:                    args,
			Perspective:             PolicyPerspectiveRequester,
			SelfDid:                 self,
			SubjectDid:              input.SubjectDid,
			CounterpartyDid:         counterparty,
			RootRequesterDid:        input.RootRequesterDid,
			RFP:                     input.RFP.api(),
			Bid:                     input.Bid.api(),
			Accept:                  input.Accept.api(),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
		},
	}, nil
}

func (e *PolicyEvaluator) runName() string {
	now := e.now().UnixNano()
	count := e.counter.Add(1)
	return fmt.Sprintf("requester-policy-%d-%d", now, count)
}

func outputBoolValue(raw json.RawMessage) (bool, bool) {
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func outputStringValues(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if trimmed := strings.TrimSpace(text); trimmed != "" && trimmed != "[]" {
			return []string{trimmed}
		}
	}
	return nil
}

func PolicyWorkflowName(name string) string {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	switch trimmed {
	case "":
		return DefaultPolicyWorkflow
	case "only-me", "only_me", "requester-only-me":
		return "requester-only-me"
	case "mutuals", "requester-mutuals":
		return "requester-mutuals"
	case "tangled-vouch", "tangled_vouch", "requester-tangled-vouch":
		return "requester-tangled-vouch"
	}
	if strings.HasPrefix(trimmed, "requester-") {
		return trimmed
	}
	return trimmed
}

func PolicyWorkflowText(name string) (string, bool) {
	data, ok := bundledactions.Workflow(PolicyWorkflowName(name))
	if !ok {
		return "", false
	}
	return string(data), true
}

func BuildPolicyRecord(name, workflow, workflowText, did string, args map[string]string, createdAt string) map[string]any {
	entry := map[string]any{"name": name}
	if len(args) > 0 {
		entry["args"] = args
	}
	workflowValue := any(workflow)
	if workflowText != "" {
		workflowValue = workflowText
	}
	return map[string]any{
		"$type":       PolicyGhaLiteNSID,
		"name":        name,
		"workflow":    workflowValue,
		"args":        args,
		"requester":   did,
		"perspective": PolicyPerspectiveRequester,
		"policies":    []any{entry},
		"createdAt":   createdAt,
	}
}

package requester

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	bundledactions "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const PolicyPerspectiveRequester = "requester"

type PolicyInput = participant.PolicyInput

type PolicyRef = participant.PolicyRef

type PolicyDecision = participant.PolicyDecision

type PolicyEvaluator struct {
	Runs *runs.Client

	Workflow string

	DID string
}

func (e *PolicyEvaluator) Scope(ctx context.Context, input PolicyInput) PolicyDecision {
	input.Mode = participant.PolicyModeScope
	return e.run(ctx, input)
}

func (e *PolicyEvaluator) Evaluate(ctx context.Context, input PolicyInput) PolicyDecision {
	input.Mode = cmp.Or(input.Mode, participant.PolicyModeFull)
	return e.run(ctx, input)
}

func (e *PolicyEvaluator) WorkflowFor(name string) string {
	if e == nil {
		return PolicyWorkflowName(name)
	}
	return PolicyWorkflowName(cmp.Or(strings.TrimSpace(name), e.Workflow))
}

func (e *PolicyEvaluator) run(ctx context.Context, input PolicyInput) PolicyDecision {
	if e == nil || e.Runs == nil {
		return PolicyDecision{Reason: "the policy run client is not configured"}
	}
	if e.Runs.Engine.Name == "" {
		return PolicyDecision{Reason: "no PolicyEngine is configured for this requester"}
	}
	workflow := e.WorkflowFor(input.Workflow)
	if e.DID == "" {
		return PolicyDecision{Reason: "the requester has no DID for the policy run"}
	}
	input.Perspective = PolicyPerspectiveRequester
	input.Mode = cmp.Or(input.Mode, participant.PolicyModeFull)
	input.SelfDid = cmp.Or(input.SelfDid, e.DID)
	input.CounterpartyDid = cmp.Or(input.CounterpartyDid, input.SubjectDid)
	return e.Runs.Run(ctx, runs.Request{WorkflowRef: workflow, Input: input.Runs()})
}

func (q *Requester) attachedPolicyWorkflow(ctx context.Context, ref commonv1.StrongRef) (string, error) {
	aturi, err := market.ParseATURI(ref.URI)
	if err != nil {
		return "", fmt.Errorf("the attached policy %s: %w", ref.URI, err)
	}
	record, err := q.repo.GetRecord(ctx, aturi.Authority().String(), aturi.Collection().String(), aturi.RecordKey().String())
	if err != nil {
		return "", fmt.Errorf("read the attached policy %s: %w", ref.URI, err)
	}
	if ref.CID != "" && record.CID != ref.CID {
		return "", fmt.Errorf("the attached policy %s changed: cid %s, want %s", ref.URI, record.CID, ref.CID)
	}
	if kind, _ := record.Value["$type"].(string); kind != PolicyGhaLiteNSID {
		return "", fmt.Errorf("the attached policy %s is %q, not gha-lite", ref.URI, kind)
	}
	name, _ := record.Value["name"].(string)
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("the attached policy %s names no policy", ref.URI)
	}
	return PolicyWorkflowName(name), nil
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

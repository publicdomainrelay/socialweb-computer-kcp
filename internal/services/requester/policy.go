package requester

import (
	"cmp"
	"context"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	bundledactions "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	PolicyPerspectiveRequester = "requester"

	PolicyModeScope = runs.ModeScope

	PolicyModeFull = runs.ModeFull
)

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

type PolicyDecision = runs.Decision

type PolicyEvaluator struct {
	Runs *runs.Client

	Workflow string

	DID string
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
	if e.Runs.Engine.Name == "" {
		return PolicyDecision{Reason: "no PolicyEngine is configured for this requester"}
	}
	workflow := cmp.Or(input.Workflow, e.Workflow)
	if workflow == "" {
		return PolicyDecision{Reason: "the requester names no policy workflow"}
	}
	if e.DID == "" {
		return PolicyDecision{Reason: "the requester has no DID for the policy run"}
	}
	return e.Runs.Run(ctx, runs.Request{WorkflowRef: workflow, Input: runs.Input{
		Perspective:      PolicyPerspectiveRequester,
		Mode:             cmp.Or(input.Mode, PolicyModeFull),
		SelfDid:          cmp.Or(input.SelfDid, e.DID),
		SubjectDid:       input.SubjectDid,
		CounterpartyDid:  cmp.Or(input.CounterpartyDid, input.SubjectDid),
		RootRequesterDid: input.RootRequesterDid,
		RFP:              input.RFP.api(),
		Bid:              input.Bid.api(),
		Accept:           input.Accept.api(),
	}})
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

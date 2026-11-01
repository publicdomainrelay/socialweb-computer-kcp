package requester

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	bundledactions "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const PolicyPerspectiveRequester = "requester"

var ErrRFPNoPolicy = errors.New("the RFP attaches no policy")

var ErrPolicyRejected = errors.New("rejected by policy")

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
	if len(input.WorkflowBody) > 0 {
		if input.WorkflowSource == "" {
			return PolicyDecision{Reason: "the attached policy workflow names no source record"}
		}
		return e.Runs.Run(ctx, runs.Request{Workflow: input.WorkflowBody, Source: input.WorkflowSource, Input: input.Runs()})
	}
	return e.Runs.Run(ctx, runs.Request{WorkflowRef: workflow, Input: input.Runs()})
}

type PolicyGate struct {
	Name string

	Source string

	CID string

	Body json.RawMessage
}

func (g PolicyGate) Key() string {
	return g.Name + "@" + g.Source + "#" + g.CID
}

func (g PolicyGate) Input(in PolicyInput) PolicyInput {
	in.Workflow = g.Name
	in.WorkflowBody = g.Body
	in.WorkflowSource = g.Source
	return in
}

func (q *Requester) attachedPolicy(ctx context.Context, ref market.StrongRef, want string) (PolicyGate, error) {
	aturi, err := market.ParseATURI(ref.URI)
	if err != nil {
		return PolicyGate{}, fmt.Errorf("the attached policy %s: %w", ref.URI, err)
	}
	if authority := aturi.Authority().String(); authority != q.repoDID {
		return PolicyGate{}, fmt.Errorf("the attached policy %s is in repo %s, not the requester's %s", ref.URI, authority, q.repoDID)
	}
	record, err := q.repo.GetRecord(ctx, aturi.Authority().String(), aturi.Collection().String(), aturi.RecordKey().String())
	if err != nil {
		return PolicyGate{}, fmt.Errorf("read the attached policy %s: %w", ref.URI, err)
	}
	if ref.CID != "" && record.CID != ref.CID {
		return PolicyGate{}, fmt.Errorf("the attached policy %s changed: cid %s, want %s", ref.URI, record.CID, ref.CID)
	}
	body, err := participant.PolicyRecordWorkflow(record.Value)
	if err != nil {
		return PolicyGate{}, fmt.Errorf("the attached policy %s: %w", ref.URI, err)
	}
	name, _ := record.Value["name"].(string)
	if strings.TrimSpace(name) == "" {
		return PolicyGate{}, fmt.Errorf("the attached policy %s names no policy", ref.URI)
	}
	gate := PolicyGate{Name: PolicyWorkflowName(name), Source: record.URI, CID: record.CID, Body: body}
	if gate.Name != PolicyWorkflowName(want) {
		return PolicyGate{}, fmt.Errorf("the contract names policy %s but its RFP attaches %s at %s", PolicyWorkflowName(want), gate.Name, ref.URI)
	}
	if text, ok := PolicyWorkflowText(gate.Name); ok {
		bundled, err := participant.PolicyRecordWorkflow(map[string]any{"$type": PolicyGhaLiteNSID, "workflow": text})
		if err != nil {
			return PolicyGate{}, fmt.Errorf("the bundled policy %s: %w", gate.Name, err)
		}
		if !bytes.Equal(bundled, body) {
			return PolicyGate{}, fmt.Errorf("the attached policy %s is not the bundled %s workflow; start a new contract or session name", ref.URI, gate.Name)
		}
	}
	return gate, nil
}

func rfpPolicyRef(record map[string]any) (market.StrongRef, error) {
	rfp, err := market.ParseRFP(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	if len(rfp.Policies) == 0 {
		return market.StrongRef{}, ErrRFPNoPolicy
	}
	if len(rfp.Policies) != 1 {
		return market.StrongRef{}, fmt.Errorf("the RFP attaches %d policies, want 1", len(rfp.Policies))
	}
	if rfp.Policies[0].URI == "" {
		return market.StrongRef{}, errors.New("the RFP attaches a policy without a uri")
	}
	return rfp.Policies[0], nil
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

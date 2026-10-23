package bidder

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	PolicyPerspectiveBidder = "bidder"

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

type PolicyDecision = runs.Decision

type PolicyRecordResolver interface {
	Resolve(ctx context.Context, ref market.RecordRef) (market.ResolvedRecord, error)
}

type PolicyEvaluator struct {
	Runs *runs.Client

	ScopePolicy string

	Records PolicyRecordResolver
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
	input.Perspective = cmp.Or(input.Perspective, PolicyPerspectiveBidder)
	input.Mode = cmp.Or(input.Mode, PolicyModeFull)
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
		decision := e.run(ctx, PolicyRunRequest{Workflow: workflow, Source: entry.URI, Input: input})
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

	Source string

	Input PolicyInput

	Timeout time.Duration
}

func (e *PolicyEvaluator) run(ctx context.Context, req PolicyRunRequest) PolicyDecision {
	if e == nil || e.Runs == nil {
		return PolicyDecision{Reason: "the policy run client is not configured"}
	}
	input := req.Input
	return e.Runs.Run(ctx, runs.Request{WorkflowRef: req.WorkflowRef, Workflow: req.Workflow, Source: req.Source, Timeout: req.Timeout,
		Input: runs.Input{Perspective: cmp.Or(input.Perspective, PolicyPerspectiveBidder), Mode: cmp.Or(input.Mode, PolicyModeFull),
			SelfDid: input.SelfDid, SubjectDid: input.SubjectDid, CounterpartyDid: input.CounterpartyDid, RootRequesterDid: input.RootRequesterDid,
			RFP: input.RFP.api(), Bid: input.Bid.api(), Accept: input.Accept.api()}})
}

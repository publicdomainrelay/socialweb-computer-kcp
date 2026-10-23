package bidder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const (
	PerspectiveBidder = "bidder"

	PolicyRecordType = "computer.socialweb.temp.policy.ghalite"
)

type policyEvaluator struct {
	runs *runs.Client

	scope string

	records *market.RepoStore
}

func (e *policyEvaluator) Scope(ctx context.Context, input participant.PolicyInput) participant.PolicyDecision {
	if e.scope == "" {
		return participant.PolicyDecision{Allow: true}
	}
	input.Perspective = PerspectiveBidder
	input.Mode = participant.PolicyModeScope
	return e.run(ctx, runs.Request{WorkflowRef: e.scope}, input)
}

func (e *policyEvaluator) EvaluatePolicies(ctx context.Context, refs []market.StrongRef, input participant.PolicyInput) participant.PolicyDecision {
	if input.Perspective == "" {
		input.Perspective = PerspectiveBidder
	}
	if input.Mode == "" {
		input.Mode = participant.PolicyModeFull
	}
	for _, entry := range refs {
		if _, err := market.ParseRecordRef(entry.URI, entry.CID); err != nil {
			return participant.PolicyDecision{Reason: fmt.Sprintf("failed to resolve policy ref %s: %v", entry.URI, err)}
		}
		record, err := e.records.Resolve(ctx, market.RecordRef{URI: entry.URI, CID: entry.CID})
		if err != nil {
			return participant.PolicyDecision{Reason: fmt.Sprintf("failed to resolve policy ref %s: %v", entry.URI, err)}
		}
		workflow, err := workflowOfRecord(record.Value)
		if err != nil {
			return participant.PolicyDecision{Reason: fmt.Sprintf("policy ref %s: %v", entry.URI, err)}
		}
		if decision := e.run(ctx, runs.Request{Workflow: workflow, Source: entry.URI}, input); !decision.Allow {
			return decision
		}
	}
	return participant.PolicyDecision{Allow: true}
}

func workflowOfRecord(value map[string]any) (json.RawMessage, error) {
	if kind, _ := value["$type"].(string); kind != PolicyRecordType {
		return nil, fmt.Errorf("the policy record type %q is not gha-lite and is denied", kind)
	}
	raw, ok := value["workflow"]
	if !ok || raw == nil {
		return nil, errors.New("the policy record names no workflow")
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, errors.New("the policy record names an empty workflow")
		}
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(v), &parsed); err != nil || parsed == nil {
			return nil, errors.New("the policy record's workflow is not a gha-lite workflow mapping")
		}
		return json.Marshal(parsed)
	case map[string]any:
		return json.Marshal(v)
	default:
		return nil, errors.New("the policy record's workflow is not a string or object")
	}
}

func (e *policyEvaluator) run(ctx context.Context, req runs.Request, input participant.PolicyInput) participant.PolicyDecision {
	if e.runs == nil {
		return participant.PolicyDecision{Reason: "the policy run client is not configured"}
	}
	req.Input = input.Runs()
	return e.runs.Run(ctx, req)
}

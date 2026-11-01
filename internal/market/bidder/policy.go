package bidder

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const (
	PerspectiveBidder = "bidder"

	PolicyRecordType = participant.PolicyRecordType
)

type policyEvaluator struct {
	runs *runs.Client

	scope string

	records *market.RepoStore
}

func (e *policyEvaluator) workflow() string {
	if e == nil {
		return v1alpha1.BidderDefaultPolicy
	}
	return cmp.Or(strings.TrimSpace(e.scope), v1alpha1.BidderDefaultPolicy)
}

func (e *policyEvaluator) Scope(ctx context.Context, input participant.PolicyInput) participant.PolicyDecision {
	input.Perspective = PerspectiveBidder
	input.Mode = participant.PolicyModeScope
	input.RFP = nil
	return e.run(ctx, runs.Request{WorkflowRef: e.workflow()}, input)
}

func (e *policyEvaluator) EvaluatePolicies(ctx context.Context, refs []market.StrongRef, input participant.PolicyInput) participant.PolicyDecision {
	if input.Perspective == "" {
		input.Perspective = PerspectiveBidder
	}
	if input.Mode == "" {
		input.Mode = participant.PolicyModeFull
	}
	if own := e.run(ctx, runs.Request{WorkflowRef: e.workflow()}, input); !own.Allow {
		return own
	}
	if len(refs) == 0 {
		return participant.PolicyDecision{Allow: true}
	}
	if e == nil || e.records == nil {
		return participant.PolicyDecision{Reason: "the policy record store is not configured"}
	}
	for _, entry := range refs {
		if _, err := market.ParseRecordRef(entry.URI, entry.CID); err != nil {
			return participant.PolicyDecision{Reason: fmt.Sprintf("failed to resolve policy ref %s: %v", entry.URI, err)}
		}
		record, err := e.records.Resolve(ctx, market.RecordRef{URI: entry.URI, CID: entry.CID})
		if err != nil {
			return participant.PolicyDecision{Reason: fmt.Sprintf("failed to resolve policy ref %s: %v", entry.URI, err)}
		}
		workflow, err := participant.PolicyRecordWorkflow(record.Value)
		if err != nil {
			return participant.PolicyDecision{Reason: fmt.Sprintf("policy ref %s: %v", entry.URI, err)}
		}
		if decision := e.run(ctx, runs.Request{Workflow: workflow, Source: entry.URI}, input); !decision.Allow {
			return decision
		}
	}
	return participant.PolicyDecision{Allow: true}
}

func (e *policyEvaluator) run(ctx context.Context, req runs.Request, input participant.PolicyInput) participant.PolicyDecision {
	if e == nil || e.runs == nil {
		return participant.PolicyDecision{Reason: "the policy run client is not configured"}
	}
	req.Input = input.Runs()
	return e.runs.Run(ctx, req)
}

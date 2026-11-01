package bidder

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const (
	PerspectiveBidder = "bidder"

	PolicyRecordType = participant.PolicyRecordType

	PolicyAllowTTL = 5 * time.Minute

	PolicyAllowMaxEntries = 4096
)

type policyEvaluator struct {
	runs *runs.Client

	scope string

	records *market.RepoStore

	allowed *ttlcache.Cache[string, participant.PolicyDecision]
}

func newPolicyEvaluator(client *runs.Client, scope string, records *market.RepoStore, ttl time.Duration) *policyEvaluator {
	return &policyEvaluator{runs: client, scope: scope, records: records,
		allowed: ttlcache.New[string, participant.PolicyDecision](ttlcache.Options{TTL: min(cmp.Or(ttl, PolicyAllowTTL), PolicyAllowTTL), MaxEntries: PolicyAllowMaxEntries})}
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
	key := policyKey(input, e.workflow())
	input.RFP = nil
	return e.cached(key, func() participant.PolicyDecision {
		return e.run(ctx, runs.Request{WorkflowRef: e.workflow()}, input)
	})
}

func (e *policyEvaluator) EvaluatePolicies(ctx context.Context, refs []market.StrongRef, input participant.PolicyInput) participant.PolicyDecision {
	if input.Perspective == "" {
		input.Perspective = PerspectiveBidder
	}
	if input.Mode == "" {
		input.Mode = participant.PolicyModeFull
	}
	set := []string{e.workflow()}
	for _, entry := range refs {
		set = append(set, entry.URI+"#"+entry.CID)
	}
	return e.cached(policyKey(input, set...), func() participant.PolicyDecision {
		return e.evaluate(ctx, refs, input)
	})
}

func (e *policyEvaluator) evaluate(ctx context.Context, refs []market.StrongRef, input participant.PolicyInput) participant.PolicyDecision {
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

func (e *policyEvaluator) cached(key string, run func() participant.PolicyDecision) participant.PolicyDecision {
	if e == nil || e.allowed == nil {
		return run()
	}
	if decision, ok := e.allowed.Get(key); ok && decision.Allow {
		return decision
	}
	decision := run()
	if decision.Allow {
		e.allowed.Set(key, decision)
	}
	return decision
}

func policyKey(input participant.PolicyInput, workflows ...string) string {
	rfp := ""
	if input.RFP != nil {
		rfp = input.RFP.URI + "#" + input.RFP.CID
	}
	return strings.Join(append([]string{input.Mode, input.Perspective, input.SelfDid, input.SubjectDid, input.CounterpartyDid,
		input.RootRequesterDid, rfp}, workflows...), "|")
}

func (e *policyEvaluator) run(ctx context.Context, req runs.Request, input participant.PolicyInput) participant.PolicyDecision {
	if e == nil || e.runs == nil {
		return participant.PolicyDecision{Reason: "the policy run client is not configured"}
	}
	req.Input = input.Runs()
	return e.runs.Run(ctx, req)
}

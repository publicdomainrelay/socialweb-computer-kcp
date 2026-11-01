package bidder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	bundledactions "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

func specBidder(h *policytest.Harness, own string) *Bidder {
	records := market.NewRepoStore(market.RepoStoreConfig{DID: "did:plc:policytestbidderlocal", Foreign: h.World})
	return &Bidder{
		cfg:     Config{DID: policytest.Bidder, ScopePolicy: own},
		records: records,
		policy:  &policyEvaluator{runs: h.Client(PerspectiveBidder), scope: own, records: records},
	}
}

func (b *Bidder) specRFPToBid(ctx context.Context, requester string, rfp policytest.Ref, attached []market.StrongRef) participant.PolicyDecision {
	if scope := b.policy.Scope(ctx, participant.PolicyInput{SelfDid: b.cfg.DID, CounterpartyDid: requester,
		RFP: &participant.PolicyRef{URI: rfp.URI, CID: rfp.CID}}); !scope.Allow {
		return scope
	}
	return b.policy.EvaluatePolicies(ctx, attached, b.fullInput(requester, rfp.URI, rfp.CID))
}

type bidderMode struct {
	own      string
	workflow string
	want     func(policytest.TrustCase) bool
}

var bidderModes = []bidderMode{
	{own: "", workflow: v1alpha1.BidderDefaultPolicy, want: func(c policytest.TrustCase) bool { return c.Vouch }},
	{own: "bidder-tangled-vouch", workflow: "bidder-tangled-vouch", want: func(c policytest.TrustCase) bool { return c.Vouch }},
	{own: "bidder-only-me", workflow: "bidder-only-me", want: func(c policytest.TrustCase) bool { return c.OnlyMe }},
	{own: "open", workflow: "open", want: func(policytest.TrustCase) bool { return true }},
}

func modeName(m bidderMode) string {
	if m.own == "" {
		return "default"
	}
	return m.own
}

type ranRun struct {
	mode     string
	workflow string
	allow    bool
	finished bool
}

func runsOf(h *policytest.Harness, mark int) []ranRun {
	out := []ranRun{}
	for _, run := range h.Runner.Since(mark) {
		allow := false
		if raw, ok := run.Status.Outputs["evaluate"]; ok {
			allow = strings.Contains(string(raw), `"allow":"true"`)
		}
		out = append(out, ranRun{mode: run.Input("mode"), workflow: run.Workflow, allow: allow, finished: run.Status.CompletionTime != nil})
	}
	return out
}

func requireRan(t *testing.T, got []ranRun, mode, workflow string) ranRun {
	t.Helper()
	for _, run := range got {
		if run.mode == mode && run.workflow == workflow {
			if !run.finished {
				t.Fatalf("the %s %s run did not finish", mode, workflow)
			}
			return run
		}
	}
	t.Fatalf("no %s run of %s for this decision; runs %+v", mode, workflow, got)
	return ranRun{}
}

func attachedRecord(h *policytest.Harness, name string) market.StrongRef {
	text, _ := bundledactions.Workflow(name)
	ref := h.World.PolicyRecord(policytest.Requester, "policy-"+name, name, string(text))
	return market.StrongRef{URI: ref.URI, CID: ref.CID}
}

func TestBidderDecisionSites(t *testing.T) {
	t.Parallel()
	for _, site := range []string{"rfp-to-bid", "accept"} {
		for _, mode := range bidderModes {
			for _, attach := range []string{"", "requester-tangled-vouch"} {
				for _, tc := range policytest.TrustCases {
					label := "none"
					if attach != "" {
						label = attach
					}
					t.Run("always-run/bidder-"+site+"/"+modeName(mode)+"/attached-"+label+"/"+tc.Name, func(t *testing.T) {
						t.Parallel()
						h := policytest.New(t)
						p := policytest.PartiesFor("bidder")
						tc.Build(h.World, p)
						b := specBidder(h, mode.own)
						var refs []market.StrongRef
						if attach != "" {
							refs = append(refs, attachedRecord(h, attach))
						}
						rfp := h.World.RFP(policytest.Requester, "rfp-"+site)
						mark := h.Runner.Mark()
						ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
						defer cancel()
						verdict := b.specRFPToBid(ctx, p.CP, rfp, refs)
						got := runsOf(h, mark)
						ownScope := requireRan(t, got, runs.ModeScope, mode.workflow)
						want := mode.want(tc)
						if ownScope.allow != want {
							t.Fatalf("the scope run of %s answered allow=%t, want %t", mode.workflow, ownScope.allow, want)
						}
						if !ownScope.allow {
							if verdict.Allow {
								t.Fatalf("the decision allowed after the scope run denied")
							}
							for _, run := range got {
								if run.mode == runs.ModeFull {
									t.Fatalf("a full run went ahead after the scope run denied")
								}
							}
							return
						}
						ownFull := requireRan(t, got, runs.ModeFull, mode.workflow)
						if ownFull.allow != want {
							t.Fatalf("the full run of %s answered allow=%t, want %t", mode.workflow, ownFull.allow, want)
						}
						if attach != "" && ownFull.allow {
							attachedRun := requireRan(t, got, runs.ModeFull, refs[0].URI)
							want = want && tc.Vouch
							if attachedRun.allow != tc.Vouch {
								t.Fatalf("the attached %s run answered allow=%t, want %t", attach, attachedRun.allow, tc.Vouch)
							}
						}
						if verdict.Allow != want {
							t.Fatalf("the %s decision allow=%t, want %t (%s)", site, verdict.Allow, want, verdict.Reason)
						}
					})
				}
			}
		}
	}
}

func TestBidderAttachedPoliciesFailClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		attach func(h *policytest.Harness) market.StrongRef
		reason string
	}{
		{"attached-deny-workflow", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Requester, "split", "split-verdict", policytest.WorkflowFixture("split-verdict"))
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "the second job denies"},
		{"attached-typescript-record", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.RecordOfType(policytest.Requester, "computer.socialweb.temp.policy.typescript", "ts", policytest.RecordFixture("typescript"))
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "not gha-lite"},
		{"attached-record-missing", func(h *policytest.Harness) market.StrongRef {
			return market.StrongRef{URI: policytest.URI(policytest.Requester, policytest.PolicyGhaLiteNSID, "gone"), CID: "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}
		}, "failed to resolve policy ref"},
		{"attached-record-cid-changed", func(h *policytest.Harness) market.StrongRef {
			ref := attachedRecord(h, "requester-tangled-vouch")
			return market.StrongRef{URI: ref.URI, CID: "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}
		}, "failed to resolve policy ref"},
	} {
		t.Run("always-run/bidder-rfp-to-bid/default/"+tc.name, func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			h.World.Vouch(policytest.Bidder, policytest.Requester)
			b := specBidder(h, "")
			rfp := h.World.RFP(policytest.Requester, "rfp")
			mark := h.Runner.Mark()
			verdict := b.specRFPToBid(context.Background(), policytest.Requester, rfp, []market.StrongRef{tc.attach(h)})
			got := runsOf(h, mark)
			requireRan(t, got, runs.ModeScope, v1alpha1.BidderDefaultPolicy)
			requireRan(t, got, runs.ModeFull, v1alpha1.BidderDefaultPolicy)
			if verdict.Allow {
				t.Fatalf("an attached policy that cannot allow let the bid through")
			}
			if !strings.Contains(verdict.Reason, tc.reason) {
				t.Fatalf("want a deny with %q, got %q", tc.reason, verdict.Reason)
			}
		})
	}
}

func TestBidderFailClosedWithoutEngine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(h *policytest.Harness, b *Bidder)
	}{
		{"no-run-client", func(h *policytest.Harness, b *Bidder) { b.policy = &policyEvaluator{} }},
		{"sandbox-unfenced", func(h *policytest.Harness, b *Bidder) { h.Sandbox.SetMode(policytest.SandboxUnfenced) }},
		{"no-engine-output", func(h *policytest.Harness, b *Bidder) {}},
	} {
		t.Run("fail-closed/bidder-rfp-to-bid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			h.World.Vouch(policytest.Bidder, policytest.Requester)
			b := specBidder(h, "open")
			tc.setup(h, b)
			rfp := h.World.RFP(policytest.Requester, "rfp")
			var attached []market.StrongRef
			if tc.name == "no-engine-output" {
				ref := h.World.PolicyRecord(policytest.Requester, "silent", "silent", policytest.WorkflowFixture("no-allow-output"))
				attached = []market.StrongRef{{URI: ref.URI, CID: ref.CID}}
			}
			verdict := b.specRFPToBid(context.Background(), policytest.Requester, rfp, attached)
			if verdict.Allow {
				t.Fatalf("the bid decision allowed with %s", tc.name)
			}
			if strings.TrimSpace(verdict.Reason) == "" {
				t.Fatalf("the deny carries no reason")
			}
		})
	}
}

func TestBidderDefaultPolicyIsTangledVouch(t *testing.T) {
	t.Parallel()
	if v1alpha1.BidderDefaultPolicy != "bidder-tangled-vouch" {
		t.Fatalf("the bidder default policy is %q, want bidder-tangled-vouch", v1alpha1.BidderDefaultPolicy)
	}
	for _, own := range []string{"", "  "} {
		if got := (&policyEvaluator{scope: own}).workflow(); got != "bidder-tangled-vouch" {
			t.Fatalf("spec.policy %q runs %q, want bidder-tangled-vouch", own, got)
		}
	}
}

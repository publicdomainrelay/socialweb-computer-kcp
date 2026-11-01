package requester

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

type specIntents struct{ contractmarket.Intents }

type specGuests struct{ Guests }

type specDirectory struct{ identity.Directory }

func specRequester(t *testing.T, h *policytest.Harness, workflow string) *Requester {
	t.Helper()
	q, err := New(Config{PolicyWorkflow: workflow, DID: policytest.Requester, RepoDID: policytest.Requester, MarketDID: policytest.Requester},
		Deps{
			Intents: specIntents{},
			Inputs:  participant.NewInputs(),
			OpenRepo: func(context.Context, string) (atprotorepo.Repo, error) {
				return nil, errors.New("no local repo in the policy spec")
			},
			Guests:    specGuests{},
			Directory: specDirectory{},
			Policy: func(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
				return h.Runner, nil
			},
			Log: slog.New(slog.DiscardHandler),
		})
	if err != nil {
		t.Fatalf("building the requester: %v", err)
	}
	q.repo = market.NewRepoStore(market.RepoStoreConfig{DID: "did:plc:policytestreqlocal", Foreign: h.World})
	q.policy = &PolicyEvaluator{Runs: h.Client(PolicyPerspectiveRequester), Workflow: PolicyWorkflowName(workflow), DID: q.marketDID}
	return q
}

func specGate(t *testing.T, h *policytest.Harness, q *Requester, contractPolicy string) PolicyGate {
	t.Helper()
	name := PolicyWorkflowName(contractPolicy)
	text, ok := PolicyWorkflowText(name)
	if !ok {
		t.Fatalf("no bundled workflow %s", name)
	}
	ref := h.World.PolicyRecord(policytest.Requester, "policy-"+name, name, text)
	gate, err := q.attachedPolicy(context.Background(), market.StrongRef{URI: ref.URI, CID: ref.CID}, contractPolicy)
	if err != nil {
		t.Fatalf("the requester refused its own bundled %s record: %v", name, err)
	}
	return gate
}

func specBid(h *policytest.Harness, bidder, rkey string, free bool, cost float64, rfp policytest.Ref) BidRecord {
	ref := h.World.Bid(bidder, rkey, free, cost)
	_, value, _ := h.World.Get(ref.URI)
	record := map[string]any{}
	for key, item := range value {
		record[key] = item
	}
	record["rfp"] = map[string]any{"uri": rfp.URI, "cid": rfp.CID}
	return BidRecord{DID: bidder, URI: ref.URI, CID: ref.CID, Record: record}
}

type requesterMode struct {
	contract string
	workflow string
	want     func(policytest.TrustCase) bool
}

var requesterModes = []requesterMode{
	{contract: "", workflow: DefaultPolicyWorkflow, want: func(c policytest.TrustCase) bool { return c.Vouch }},
	{contract: "requester-tangled-vouch", workflow: "requester-tangled-vouch", want: func(c policytest.TrustCase) bool { return c.Vouch }},
	{contract: "requester-only-me", workflow: "requester-only-me", want: func(c policytest.TrustCase) bool { return c.OnlyMe }},
	{contract: "requester-mutuals", workflow: "requester-mutuals", want: func(c policytest.TrustCase) bool { return c.Mutuals }},
	{contract: "open", workflow: "open", want: func(policytest.TrustCase) bool { return true }},
}

func (m requesterMode) label() string {
	if m.contract == "" {
		return "default"
	}
	return m.contract
}

func gateRuns(h *policytest.Harness, mark int, gate PolicyGate, mode string) []*policytest.Run {
	var out []*policytest.Run
	for _, run := range h.Runner.Since(mark) {
		if run.Workflow == gate.Source && run.Input("mode") == mode {
			out = append(out, run)
		}
	}
	return out
}

func ranAllow(t *testing.T, run *policytest.Run) bool {
	t.Helper()
	if run.Status.CompletionTime == nil {
		t.Fatalf("the policy run %s did not finish", run.Name)
	}
	raw, ok := run.Status.Outputs["evaluate"]
	if !ok {
		t.Fatalf("the policy run %s has no evaluate output: %s", run.Name, run.Status.ConsoleOutput)
	}
	return strings.Contains(string(raw), `"allow":"true"`)
}

func TestRequesterDecisionSites(t *testing.T) {
	t.Parallel()
	for _, mode := range requesterModes {
		for _, tc := range policytest.TrustCases {
			want := mode.want(tc)
			t.Run("always-run/requester-free-winner/"+mode.label()+"/scope/"+tc.Name, func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				tc.Build(h.World, policytest.PartiesFor("requester"))
				q := specRequester(t, h, mode.contract)
				gate := specGate(t, h, q, mode.contract)
				rfp := h.World.RFP(policytest.Requester, "rfp")
				bid := specBid(h, policytest.Bidder, "bid-free", true, 0, rfp)
				mark := h.Runner.Mark()
				selected := (&contractRunner{q: q}).freeWinner(context.Background(), map[string]BidRecord{bid.URI: bid}, gate)
				ran := gateRuns(h, mark, gate, runs.ModeScope)
				if len(ran) != 1 {
					t.Fatalf("want one scope run of %s for the free bid, got %d", mode.workflow, len(ran))
				}
				if got := ranAllow(t, ran[0]); got != want {
					t.Fatalf("the scope run answered allow=%t, want %t", got, want)
				}
				if (selected != nil) != want {
					t.Fatalf("free winner selected=%t, want %t", selected != nil, want)
				}
			})
			t.Run("always-run/requester-best-bid/"+mode.label()+"/scope/"+tc.Name, func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				tc.Build(h.World, policytest.PartiesFor("requester"))
				q := specRequester(t, h, mode.contract)
				gate := specGate(t, h, q, mode.contract)
				rfp := h.World.RFP(policytest.Requester, "rfp")
				bid := specBid(h, policytest.Bidder, "bid-paid", false, 2, rfp)
				mark := h.Runner.Mark()
				selected := (&contractRunner{q: q}).bestBid(context.Background(), map[string]BidRecord{bid.URI: bid}, gate)
				ran := gateRuns(h, mark, gate, runs.ModeScope)
				if len(ran) != 1 {
					t.Fatalf("want one scope run of %s for the paid bid, got %d", mode.workflow, len(ran))
				}
				if got := ranAllow(t, ran[0]); got != want {
					t.Fatalf("the scope run answered allow=%t, want %t", got, want)
				}
				if (selected != nil) != want {
					t.Fatalf("best bid selected=%t, want %t", selected != nil, want)
				}
			})
			for _, free := range []bool{true, false} {
				kind := "paid"
				if free {
					kind = "free"
				}
				t.Run("always-run/requester-session-pick-"+kind+"/"+mode.label()+"/scope+full/"+tc.Name, func(t *testing.T) {
					t.Parallel()
					h := policytest.New(t)
					tc.Build(h.World, policytest.PartiesFor("requester"))
					q := specRequester(t, h, mode.contract)
					gate := specGate(t, h, q, mode.contract)
					rfp := h.World.RFP(policytest.Requester, "rfp")
					bid := specBid(h, policytest.Bidder, "bid-"+kind, free, 2, rfp)
					mark := h.Runner.Mark()
					best := q.sessionBestBid(context.Background(), market.SignedRecord{URI: rfp.URI, CID: rfp.CID}, map[string]BidRecord{bid.URI: bid},
						gate, map[string]bool{}, 5)
					scoped := gateRuns(h, mark, gate, runs.ModeScope)
					if len(scoped) == 0 {
						t.Fatalf("the session pick ran no scope policy")
					}
					scopeAllow := ranAllow(t, scoped[0])
					if scopeAllow != want {
						t.Fatalf("the scope run answered allow=%t, want %t", scopeAllow, want)
					}
					fulls := gateRuns(h, mark, gate, runs.ModeFull)
					switch {
					case !scopeAllow && len(fulls) != 0:
						t.Fatalf("a full run went ahead after the scope run denied")
					case scopeAllow && len(fulls) != 1:
						t.Fatalf("want one full run for the picked bid, got %d", len(fulls))
					case scopeAllow && ranAllow(t, fulls[0]) != want:
						t.Fatalf("the full run answered allow=%t, want %t", !want, want)
					}
					if (best != nil) != want {
						t.Fatalf("session pick selected=%t, want %t", best != nil, want)
					}
				})
			}
		}
	}
}

func TestRequesterAttachedPolicyGate(t *testing.T) {
	t.Parallel()
	tangled, _ := PolicyWorkflowText("requester-tangled-vouch")
	for _, tc := range []struct {
		name     string
		contract string
		record   func(h *policytest.Harness) market.StrongRef
		reason   string
	}{
		{"own-bundled-record-accepted", "requester-tangled-vouch", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Requester, "p", "requester-tangled-vouch", tangled)
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, ""},
		{"foreign-repo-denied", "requester-tangled-vouch", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Stranger, "p", "requester-tangled-vouch", tangled)
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "not the requester's"},
		{"bundled-name-other-body-denied", "requester-tangled-vouch", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Requester, "p", "requester-tangled-vouch", policytest.WorkflowFixture("literal-allow"))
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "is not the bundled"},
		{"other-policy-name-denied", "requester-only-me", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Requester, "p", "requester-tangled-vouch", tangled)
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "but its RFP attaches"},
		{"changed-cid-denied", "requester-tangled-vouch", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Requester, "p", "requester-tangled-vouch", tangled)
			h.World.PolicyRecord(policytest.Requester, "p", "requester-tangled-vouch", tangled+"\n")
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "changed"},
		{"typescript-record-denied", "requester-tangled-vouch", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.RecordOfType(policytest.Requester, PolicyGhaLiteNSID, "ts", policytest.RecordFixture("typescript"))
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "not gha-lite"},
		{"empty-contract-policy-is-tangled-vouch", "", func(h *policytest.Harness) market.StrongRef {
			ref := h.World.PolicyRecord(policytest.Requester, "p", "requester-tangled-vouch", tangled)
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, ""},
		{"empty-contract-policy-refuses-open", "", func(h *policytest.Harness) market.StrongRef {
			text, _ := PolicyWorkflowText("open")
			ref := h.World.PolicyRecord(policytest.Requester, "p", "open", text)
			return market.StrongRef{URI: ref.URI, CID: ref.CID}
		}, "but its RFP attaches open"},
	} {
		t.Run("always-run/requester-attached-gate/"+tc.name, func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			q := specRequester(t, h, "")
			gate, err := q.attachedPolicy(context.Background(), tc.record(h), tc.contract)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("the gate refused: %v", err)
				}
				if gate.Name != DefaultPolicyWorkflow && tc.contract == "" {
					t.Fatalf("an unnamed contract policy gates on %s", gate.Name)
				}
				return
			}
			if err == nil {
				t.Fatalf("the gate accepted the record %+v", gate)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want a refusal with %q, got %v", tc.reason, err)
			}
		})
	}
}

func TestRequesterDefaultPolicyRuns(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", " ", "tangled-vouch", "TANGLED-VOUCH"} {
		if got := PolicyWorkflowName(name); got != "requester-tangled-vouch" {
			t.Fatalf("policy %q maps to %q, want requester-tangled-vouch", name, got)
		}
	}
	for _, mode := range []string{runs.ModeScope, runs.ModeFull} {
		t.Run("always-run/requester-evaluator/default/"+mode+"/unvouched-denies", func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			q := specRequester(t, h, "")
			input := PolicyInput{SubjectDid: policytest.Bidder, CounterpartyDid: policytest.Bidder, RootRequesterDid: policytest.Requester, Mode: mode}
			mark := h.Runner.Mark()
			decision := q.policy.Evaluate(context.Background(), input)
			ran := h.Runner.Since(mark)
			if len(ran) != 1 || ran[0].Workflow != "requester-tangled-vouch" {
				t.Fatalf("the default evaluation did not run requester-tangled-vouch: %d runs", len(ran))
			}
			if decision.Allow {
				t.Fatalf("an unvouched bidder was allowed by the default policy")
			}
		})
	}
}

func TestRequesterScopeCache(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*policytest.Harness, *Requester, PolicyGate, BidRecord) {
		h := policytest.New(t)
		h.World.Vouch(policytest.Requester, policytest.Bidder)
		q := specRequester(t, h, "")
		gate := specGate(t, h, q, "")
		rfp := h.World.RFP(policytest.Requester, "rfp")
		return h, q, gate, specBid(h, policytest.Bidder, "bid", true, 0, rfp)
	}
	t.Run("cache/requester-scope/default/scope/hit-still-runs-same-verdict", func(t *testing.T) {
		t.Parallel()
		h, q, gate, bid := setup(t)
		first := q.bidAllowedWithPolicy(context.Background(), bid, gate)
		mark := h.Runner.Mark()
		second := q.bidAllowedWithPolicy(context.Background(), bid, gate)
		if first != second || !first {
			t.Fatalf("the cached scope verdict %t differs from the uncached %t", second, first)
		}
		if len(gateRuns(h, mark, gate, runs.ModeScope)) != 1 {
			t.Fatalf("a scope decision served from the requester cache recorded no engine run")
		}
	})
	t.Run("cache/requester-scope/default/scope/other-bidder-never-hits", func(t *testing.T) {
		t.Parallel()
		h, q, gate, bid := setup(t)
		if !q.bidAllowedWithPolicy(context.Background(), bid, gate) {
			t.Fatalf("the vouched bidder was denied")
		}
		rfp := h.World.RFP(policytest.Requester, "rfp")
		other := specBid(h, policytest.Stranger, "bid-other", true, 0, rfp)
		mark := h.Runner.Mark()
		if q.bidAllowedWithPolicy(context.Background(), other, gate) {
			t.Fatalf("an unvouched bidder was served the vouched bidder's allow")
		}
		if len(gateRuns(h, mark, gate, runs.ModeScope)) != 1 {
			t.Fatalf("another bidder's decision ran no policy")
		}
	})
	t.Run("cache/requester-scope/default/scope/ttl-expires", func(t *testing.T) {
		t.Parallel()
		h, q, gate, bid := setup(t)
		q.bidAllowedWithPolicy(context.Background(), bid, gate)
		time.Sleep(31 * time.Second)
		h.World.Unvouch(policytest.Requester, policytest.Bidder)
		h.ForgetTrust()
		mark := h.Runner.Mark()
		if q.bidAllowedWithPolicy(context.Background(), bid, gate) {
			t.Fatalf("an expired cached allow was served after the vouch was removed")
		}
		if len(gateRuns(h, mark, gate, runs.ModeScope)) != 1 {
			t.Fatalf("the decision after expiry ran no policy")
		}
	})
	t.Run("cache/requester-scope/default/scope/deny-stays-deny", func(t *testing.T) {
		t.Parallel()
		h := policytest.New(t)
		q := specRequester(t, h, "")
		gate := specGate(t, h, q, "")
		rfp := h.World.RFP(policytest.Requester, "rfp")
		bid := specBid(h, policytest.Bidder, "bid", true, 0, rfp)
		if q.bidAllowedWithPolicy(context.Background(), bid, gate) || q.bidAllowedWithPolicy(context.Background(), bid, gate) {
			t.Fatalf("an unvouched bidder was allowed")
		}
	})
}

package spec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

func inline(t *testing.T, h *policytest.Harness, fixture string, mode lane) decision {
	t.Helper()
	body, err := json.Marshal(policytest.WorkflowFixture(fixture))
	if err != nil {
		t.Fatal(err)
	}
	source := policytest.URI(policytest.Requester, policytest.PolicyGhaLiteNSID, fixture)
	return run(t, h, "bidder", runs.Request{Workflow: body, Source: source, Input: runs.Input{Perspective: "bidder", Mode: string(mode),
		SelfDid: policytest.Bidder, SubjectDid: policytest.Requester, CounterpartyDid: policytest.Requester, RootRequesterDid: policytest.Requester}})
}

func TestAlwaysRunOpen(t *testing.T) {
	t.Parallel()
	for _, perspective := range []side{bidderSide, requesterSide} {
		for _, mode := range []lane{scope, full} {
			t.Run("always-run/engine/open/"+string(mode)+"/"+string(perspective), func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				p := policytest.PartiesFor(string(perspective))
				before := h.Sandbox.Actions()
				got := evaluate(t, h, "open", perspective, mode, p.Self, p.CP, nil)
				ran := got.mustRun(t, "open")
				actionRan(t, ran, "open", true)
				if h.Sandbox.Actions() == before {
					t.Fatalf("open allowed without running its action in the sandbox")
				}
				if ran.Status.Phase != policyv1alpha1.PolicyWorkflowRunSucceeded {
					t.Fatalf("the open run ended %s", ran.Status.Phase)
				}
				got.mustAllow(t)
			})
		}
	}
}

func TestWorkPolicies(t *testing.T) {
	t.Parallel()
	t.Run("matrix/under-4-cpus/scope/bidder/no-demand", func(t *testing.T) {
		t.Parallel()
		h := policytest.New(t)
		got := evaluate(t, h, "under-4-cpus", bidderSide, scope, policytest.Bidder, policytest.Requester, nil)
		actionRan(t, got.mustRun(t, "under-4-cpus"), "under-4-cpus", true)
		got.mustAllow(t)
	})
	t.Run("matrix/under-4-cpus/full/bidder/rfp-without-payload", func(t *testing.T) {
		t.Parallel()
		h := policytest.New(t)
		rfp := h.World.RFP(policytest.Requester, "rfp-plain")
		got := evaluate(t, h, "under-4-cpus", bidderSide, full, policytest.Bidder, policytest.Requester, &rfp)
		actionRan(t, got.mustRun(t, "under-4-cpus"), "under-4-cpus", true)
		got.mustAllow(t)
	})
	t.Run("matrix/under-4-cpus/full/bidder/payload-unreadable-denies", func(t *testing.T) {
		t.Parallel()
		h := policytest.New(t)
		uri := policytest.URI(policytest.Requester, market.RFPNSID, "rfp-cpus")
		cid := h.World.Put(uri, map[string]any{"$type": market.RFPNSID, "payload": map[string]any{
			"uri": policytest.URI(policytest.Requester, "computer.socialweb.temp.compute.vm", "vm-8"), "cid": "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}})
		rfp := policytest.Ref{URI: uri, CID: cid}
		got := evaluate(t, h, "under-4-cpus", bidderSide, full, policytest.Bidder, policytest.Requester, &rfp)
		actionRan(t, got.mustRun(t, "under-4-cpus"), "under-4-cpus", false)
		got.mustDeny(t, "failed to resolve demand payload")
	})
	for _, tc := range []struct {
		name  string
		free  bool
		allow bool
	}{{"free-bid", true, true}, {"paid-bid-not-allowed", false, false}} {
		t.Run("matrix/bid-payload/full/requester/"+tc.name, func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			bid := h.World.Bid(policytest.Bidder, "bid-"+tc.name, tc.free, 3)
			got := run(t, h, "requester", runs.Request{WorkflowRef: "bid-payload", Input: runs.Input{Perspective: "requester", Mode: runs.ModeFull,
				SelfDid: policytest.Requester, SubjectDid: policytest.Bidder, CounterpartyDid: policytest.Bidder, RootRequesterDid: policytest.Requester,
				Bid: &commonv1.StrongRef{URI: bid.URI, CID: bid.CID}}})
			actionRan(t, got.mustRun(t, "bid-payload"), "bid-payload", tc.allow)
			if tc.allow {
				got.mustAllow(t)
			} else {
				got.mustDeny(t, "not allowed")
			}
		})
	}
}

func TestStrictOutputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		allow   bool
		reason  string
	}{
		{"literal-allow", true, ""},
		{"no-allow-output", false, "no allow output"},
		{"malformed-allow", false, "policy rejected"},
		{"split-verdict", false, "the second job denies"},
		{"run-step", false, "net-only"},
	} {
		for _, mode := range []lane{scope, full} {
			t.Run("always-run/engine/"+tc.fixture+"/"+string(mode), func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				got := inline(t, h, tc.fixture, mode)
				got.mustRun(t, policytest.URI(policytest.Requester, policytest.PolicyGhaLiteNSID, tc.fixture))
				if tc.allow {
					got.mustAllow(t)
				} else {
					got.mustDenyBecause(t, tc.reason)
				}
			})
		}
	}
}

type workflowCRs map[string]*policyv1alpha1.PolicyWorkflow

func (w workflowCRs) GetPolicyWorkflow(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflow, error) {
	if item, ok := w[name]; ok {
		return item, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: policyv1alpha1.GroupName, Resource: "policyworkflows"}, name)
}

func TestRecordTypesFailClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		collection string
		value      func() map[string]any
		allow      bool
		reason     string
	}{
		{"gha-lite-record-runs", policytest.PolicyGhaLiteNSID, func() map[string]any {
			return map[string]any{"$type": policytest.PolicyGhaLiteNSID, "name": "literal-allow", "workflow": policytest.WorkflowFixture("literal-allow")}
		}, true, ""},
		{"typescript-record-denies", "computer.socialweb.temp.policy.typescript", func() map[string]any { return policytest.RecordFixture("typescript") },
			false, "typescript policy records are disabled"},
		{"denoworker-record-denies", "com.publicdomainrelay.temp.market.policies.denoWorker", func() map[string]any { return policytest.RecordFixture("denoworker") },
			false, "denoWorker policy records are disabled"},
		{"unknown-record-type-denies", "computer.socialweb.temp.policy.wasm", func() map[string]any { return policytest.RecordFixture("unknown-type") },
			false, "unknown policy record type"},
	} {
		t.Run("always-run/engine/record/"+tc.name, func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			ref := h.World.RecordOfType(policytest.Requester, tc.collection, "attached", tc.value())
			h.SetWorkflows(workflowCRs{"attached": {ObjectMeta: metav1.ObjectMeta{Name: "attached"},
				Spec: policyv1alpha1.PolicyWorkflowSpec{RecordRef: &commonv1.StrongRef{URI: ref.URI, CID: ref.CID}}}})
			got := evaluate(t, h, "attached", bidderSide, full, policytest.Bidder, policytest.Requester, nil)
			ran := got.mustRun(t, "attached")
			if tc.allow {
				got.mustAllow(t)
				return
			}
			got.mustDeny(t, tc.reason)
			if ran.Status.Phase != policyv1alpha1.PolicyWorkflowRunFailed || ran.Status.ConsoleOutput != "" {
				t.Fatalf("a refused record type must fail before any code runs: phase %s console %q", ran.Status.Phase, ran.Status.ConsoleOutput)
			}
		})
	}
}

func TestCacheKeyOnlyWhenDeclared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture  string
		declared bool
	}{{"cache-key-declared", true}, {"cache-key-undeclared", false}} {
		t.Run("cache/engine/cache-key/"+tc.fixture, func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			got := inline(t, h, tc.fixture, full)
			ran := got.mustRun(t, policytest.URI(policytest.Requester, policytest.PolicyGhaLiteNSID, tc.fixture))
			got.mustAllow(t)
			sent := ran.Input("cache-key")
			if sent == "" {
				t.Fatalf("the run client sent no cache-key")
			}
			seen, _ := ran.Output("evaluate", "cache-key")
			switch {
			case tc.declared && !strings.HasPrefix(seen, sent):
				t.Fatalf("a workflow that declares cache-key saw %q, want the run's key %q", seen, sent)
			case !tc.declared && seen != "absent":
				t.Fatalf("a workflow that does not declare cache-key saw %q", seen)
			}
		})
	}
	t.Run("cache/engine/cache-key/names-every-party", func(t *testing.T) {
		t.Parallel()
		h := policytest.New(t)
		a := evaluate(t, h, "open", bidderSide, full, policytest.Bidder, policytest.Requester, nil).mustRun(t, "open")
		b := evaluate(t, h, "open", bidderSide, full, policytest.Bidder, policytest.Stranger, nil).mustRun(t, "open")
		c := evaluate(t, h, "open", bidderSide, scope, policytest.Bidder, policytest.Requester, nil).mustRun(t, "open")
		if a.Input("cache-key") == b.Input("cache-key") || a.Input("cache-key") == c.Input("cache-key") {
			t.Fatalf("two decisions for other parties or lanes share the cache-key %q", a.Input("cache-key"))
		}
	})
}

type failingVouches struct {
	*trust.Resolver
}

func (failingVouches) Vouches(ctx context.Context, voucher, subject string) (bool, error) {
	return false, errors.New("the trust host is unavailable")
}

func TestFailClosed(t *testing.T) {
	t.Parallel()
	vouched := func(h *policytest.Harness) {
		h.World.Vouch(policytest.Bidder, policytest.Requester)
		h.World.Vouch(policytest.Bidder, policytest.Stranger)
	}
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, h *policytest.Harness)
		reason string
	}{
		{"sandbox-unfenced", func(t *testing.T, h *policytest.Harness) { h.Sandbox.SetMode(policytest.SandboxUnfenced) }, "enforces no fence"},
		{"sandbox-unavailable", func(t *testing.T, h *policytest.Harness) { h.Sandbox.SetMode(policytest.SandboxUnavailable) }, "unavailable"},
		{"trust-host-error", func(t *testing.T, h *policytest.Harness) { h.SetHost(failingVouches{h.Resolver}) }, "the trust host is unavailable"},
		{"pds-outage", func(t *testing.T, h *policytest.Harness) { h.World.SetDown(true) }, ""},
		{"run-not-created", func(t *testing.T, h *policytest.Harness) { h.FailResolve(errors.New("no policy engine on the bus")) }, "could not be created"},
		{"deadline", func(t *testing.T, h *policytest.Harness) {
			h.Timeout = 2 * time.Second
			h.Sandbox.SetMode(policytest.SandboxHung)
		}, "deadline"},
		{"cancelled", func(t *testing.T, h *policytest.Harness) {
			h.Sandbox.SetMode(policytest.SandboxHung)
			started := h.Sandbox.HungStarted()
			go func() {
				<-started
				h.Runner.CancelAll()
			}()
		}, "cancelled"},
	} {
		for _, workflow := range []string{"bidder-tangled-vouch", "open"} {
			for _, mode := range []lane{scope, full} {
				t.Run("fail-closed/"+tc.name+"/"+workflow+"/"+string(mode), func(t *testing.T) {
					t.Parallel()
					h := policytest.New(t)
					vouched(h)
					control := evaluate(t, h, workflow, bidderSide, mode, policytest.Bidder, policytest.Requester, nil)
					control.mustRun(t, workflow)
					control.mustAllow(t)
					h.ForgetTrust()
					tc.break_(t, h)
					got := evaluate(t, h, workflow, bidderSide, mode, policytest.Bidder, policytest.Stranger, nil)
					switch tc.name {
					case "run-not-created":
						if len(got.runs) != 0 {
							t.Fatalf("a run was recorded although the engine refused it")
						}
					case "deadline":
						if len(got.runs) != 1 {
							t.Fatalf("want one started run, got %d", len(got.runs))
						}
					default:
						got.mustRun(t, workflow)
					}
					if tc.name == "pds-outage" && workflow == "open" {
						got.mustAllow(t)
						return
					}
					got.mustDenyBecause(t, tc.reason)
				})
			}
		}
	}
}

func TestNoTrustSnapshotDenies(t *testing.T) {
	t.Parallel()
	for _, perspective := range []side{bidderSide, requesterSide} {
		for _, family := range []string{"tangled-vouch", "mutuals", "only-me", "open"} {
			for _, mode := range []lane{scope, full} {
				workflow := workflowOf(family, perspective)
				t.Run("fail-closed/no-trust-snapshot/"+workflow+"/"+string(mode)+"/"+string(perspective), func(t *testing.T) {
					t.Parallel()
					h := policytest.New(t)
					p := policytest.PartiesFor(string(perspective))
					h.World.Associate(p.CP, p.Self)
					h.World.Vouch(p.Self, p.CP)
					h.World.Vouch(p.CP, p.Self)
					h.SetHost(nil)
					before := h.Sandbox.Actions()
					got := evaluate(t, h, workflow, perspective, mode, p.Self, p.CP, nil)
					ran := got.mustRun(t, workflow)
					if h.Sandbox.Actions() == before {
						t.Fatalf("the %s action did not run", workflow)
					}
					if _, seeded := ran.Status.Cache["trust/"+p.CP]; seeded {
						t.Fatalf("a trust snapshot was seeded without a trust host")
					}
					if family == "open" {
						got.mustAllow(t)
						return
					}
					got.mustDeny(t, "")
				})
			}
		}
	}
}

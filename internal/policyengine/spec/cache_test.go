package spec

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const verdictTTL = 30 * time.Second

func TestEngineVerdictCache(t *testing.T) {
	t.Parallel()
	for _, perspective := range []side{bidderSide, requesterSide} {
		workflow := string(perspective) + "-tangled-vouch"
		p := policytest.PartiesFor(string(perspective))
		for _, vouched := range []bool{true, false} {
			name := "deny"
			if vouched {
				name = "allow"
			}
			t.Run("cache/engine/"+workflow+"/full/hit-same-verdict-"+name, func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				if vouched {
					h.World.Vouch(p.Self, p.CP)
				}
				first := evaluate(t, h, workflow, perspective, full, p.Self, p.CP, nil)
				firstRun := first.mustRun(t, workflow)
				if actionRan(t, firstRun, workflow, vouched).FromCache {
					t.Fatalf("the first decision was served from a cache")
				}
				second := evaluate(t, h, workflow, perspective, full, p.Self, p.CP, nil)
				secondRun := second.mustRun(t, workflow)
				if !actionRan(t, secondRun, workflow, vouched).FromCache {
					t.Fatalf("the repeated decision was not served from the verdict cache")
				}
				if first.verdict.Allow != second.verdict.Allow {
					t.Fatalf("the cached verdict allow=%t differs from the uncached allow=%t", second.verdict.Allow, first.verdict.Allow)
				}
			})
		}
		t.Run("cache/engine/"+workflow+"/full/other-counterparty-never-hits", func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			h.World.Vouch(p.Self, p.CP)
			evaluate(t, h, workflow, perspective, full, p.Self, p.CP, nil).mustAllow(t)
			other := evaluate(t, h, workflow, perspective, full, p.Self, policytest.Stranger, nil)
			if actionRan(t, other.mustRun(t, workflow), workflow, false).FromCache {
				t.Fatalf("another counterparty was served the first counterparty's verdict")
			}
			other.mustDeny(t, "not vouched")
		})
		t.Run("cache/engine/"+workflow+"/full/other-rfp-never-hits", func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			h.World.Vouch(p.Self, p.CP)
			first := h.World.RFP(policytest.Requester, "rfp-one")
			second := h.World.RFP(policytest.Requester, "rfp-two")
			evaluate(t, h, workflow, perspective, full, p.Self, p.CP, &first).mustAllow(t)
			again := evaluate(t, h, workflow, perspective, full, p.Self, p.CP, &second)
			if actionRan(t, again.mustRun(t, workflow), workflow, true).FromCache {
				t.Fatalf("a decision on another RFP was served from the first RFP's verdict")
			}
		})
		for _, mode := range []lane{scope, full} {
			t.Run("cache/engine/"+workflow+"/"+string(mode)+"/revoked-vouch-after-invalidation-never-hits", func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				h.World.Vouch(p.Self, p.CP)
				evaluate(t, h, workflow, perspective, mode, p.Self, p.CP, nil).mustAllow(t)
				h.World.Unvouch(p.Self, p.CP)
				h.ForgetTrust()
				again := evaluate(t, h, workflow, perspective, mode, p.Self, p.CP, nil)
				again.mustRun(t, workflow)
				again.mustDeny(t, "")
			})
			t.Run("cache/engine/"+workflow+"/"+string(mode)+"/deny-stays-deny", func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				evaluate(t, h, workflow, perspective, mode, p.Self, p.CP, nil).mustDeny(t, "")
				again := evaluate(t, h, workflow, perspective, mode, p.Self, p.CP, nil)
				again.mustRun(t, workflow)
				again.mustDeny(t, "")
			})
		}
		t.Run("cache/engine/"+workflow+"/full/verdict-ttl-expires", func(t *testing.T) {
			t.Parallel()
			h := policytest.New(t)
			h.World.Vouch(p.Self, p.CP)
			evaluate(t, h, workflow, perspective, full, p.Self, p.CP, nil).mustAllow(t)
			time.Sleep(verdictTTL + time.Second)
			again := evaluate(t, h, workflow, perspective, full, p.Self, p.CP, nil)
			if actionRan(t, again.mustRun(t, workflow), workflow, true).FromCache {
				t.Fatalf("a verdict older than %s was served from the cache", verdictTTL)
			}
		})
		t.Run("cache/engine/"+workflow+"/scope/trust-ttl-expires", func(t *testing.T) {
			t.Parallel()
			h := policytest.NewWith(t, policytest.Options{TrustTTL: 2 * time.Second})
			h.World.Vouch(p.Self, p.CP)
			evaluate(t, h, workflow, perspective, scope, p.Self, p.CP, nil).mustAllow(t)
			h.World.Unvouch(p.Self, p.CP)
			cached := evaluate(t, h, workflow, perspective, scope, p.Self, p.CP, nil)
			cached.mustRun(t, workflow)
			cached.mustAllow(t)
			time.Sleep(2500 * time.Millisecond)
			expired := evaluate(t, h, workflow, perspective, scope, p.Self, p.CP, nil)
			expired.mustRun(t, workflow)
			expired.mustDeny(t, "")
		})
	}
	t.Run("cache/engine/attached/full/other-record-body-other-key", func(t *testing.T) {
		t.Parallel()
		h := policytest.New(t)
		source := policytest.URI(policytest.Requester, policytest.PolicyGhaLiteNSID, "attached")
		keyOf := func(text string) string {
			body, _ := json.Marshal(text)
			got := run(t, h, "bidder", runs.Request{Workflow: body, Source: source, Input: runs.Input{Perspective: "bidder", Mode: runs.ModeFull,
				SelfDid: policytest.Bidder, SubjectDid: policytest.Requester, CounterpartyDid: policytest.Requester}})
			got.mustAllow(t)
			key, _ := got.mustRun(t, source).Output("evaluate", "cache-key")
			return key
		}
		original := policytest.WorkflowFixture("cache-key-declared")
		if keyOf(original) == keyOf(original+"\n# changed body\n") {
			t.Fatalf("two policy bodies under one record uri share a cache-key")
		}
	})
}

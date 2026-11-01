package requester

import (
	"context"
	"errors"
	"testing"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
)

func recordedContract(h *policytest.Harness) *v1alpha1.NodeClaimContract {
	text, _ := PolicyWorkflowText(DefaultPolicyWorkflow)
	policyRef := h.World.PolicyRecord(policytest.Requester, "policy-recorded", DefaultPolicyWorkflow, text)
	rfp := h.World.RFP(policytest.Requester, "rfp-recorded", policyRef)
	bid := h.World.Bid(policytest.Bidder, "bid-recorded", true, 0)
	return &v1alpha1.NodeClaimContract{
		Winner: &v1alpha1.ContractWinner{DID: policytest.Bidder},
		Policy: &commonv1.StrongRef{URI: policyRef.URI, CID: policyRef.CID},
		RFP:    &commonv1.StrongRef{URI: rfp.URI, CID: rfp.CID},
		Bid:    &commonv1.StrongRef{URI: bid.URI, CID: bid.CID},
	}
}

func TestRequesterRecordedWinnerRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setup   func(h *policytest.Harness)
		ran     bool
		release bool
		allow   bool
	}{
		{"completed-allow-resumes", func(h *policytest.Harness) { h.World.Vouch(policytest.Requester, policytest.Bidder) }, true, false, true},
		{"completed-deny-releases", func(h *policytest.Harness) {}, true, true, false},
		{"failed-run-keeps", func(h *policytest.Harness) {
			h.World.Vouch(policytest.Requester, policytest.Bidder)
			h.Sandbox.SetMode(policytest.SandboxUnfenced)
		}, true, false, false},
		{"cancelled-run-keeps", func(h *policytest.Harness) {
			h.World.Vouch(policytest.Requester, policytest.Bidder)
			h.Sandbox.SetMode(policytest.SandboxHung)
			started := h.Sandbox.HungStarted()
			go func() {
				<-started
				h.Runner.CancelAll()
			}()
		}, true, false, false},
		{"wait-error-keeps", func(h *policytest.Harness) {
			h.World.Vouch(policytest.Requester, policytest.Bidder)
			h.Timeout = 2 * time.Second
			h.Sandbox.SetMode(policytest.SandboxHung)
		}, true, false, false},
		{"run-not-created-keeps", func(h *policytest.Harness) {
			h.World.Vouch(policytest.Requester, policytest.Bidder)
			h.FailResolve(errors.New("no policy engine on the bus"))
		}, false, false, false},
	} {
		for _, site := range []string{"resumed-contract", "held-session"} {
			t.Run("always-run/requester-"+site+"/default/full/"+tc.name, func(t *testing.T) {
				t.Parallel()
				h := policytest.New(t)
				state := recordedContract(h)
				tc.setup(h)
				q := specRequester(t, h, "")
				mark := h.Runner.Mark()
				var err error
				if site == "held-session" {
					q.contracts.Put("held", v1alpha1.ComputeContract{Status: v1alpha1.ComputeContractStatus{Contract: state}})
					err = q.heldSessionAllowed(context.Background(), "held", "")
				} else {
					err = q.recordedWinnerAllowed(context.Background(), state, "")
				}
				runs := h.Runner.Since(mark)
				if tc.ran && len(runs) != 1 {
					t.Fatalf("want one policy run for the recorded winner, got %d", len(runs))
				}
				if !tc.ran && len(runs) != 0 {
					t.Fatalf("a run was recorded although the engine refused it")
				}
				if tc.ran && runs[0].Workflow != state.Policy.URI {
					t.Fatalf("the recorded winner ran %s, want the attached policy %s", runs[0].Workflow, state.Policy.URI)
				}
				switch {
				case tc.allow && err != nil:
					t.Fatalf("an allowed recorded winner was refused: %v", err)
				case !tc.allow && err == nil:
					t.Fatalf("the recorded winner went ahead without an allow")
				case !tc.allow && errors.Is(err, ErrPolicyRejected) != tc.release:
					t.Fatalf("release=%t for %s, want %t: %v", errors.Is(err, ErrPolicyRejected), tc.name, tc.release, err)
				}
			})
		}
	}
}

package spec

import (
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type parties struct {
	self, selfOp, selfSvc string
	cp, cpOp, cpSvc       string
}

func partiesFor(perspective side) parties {
	if perspective == requesterSide {
		return parties{self: policytest.Requester, selfOp: policytest.ReqOperator, selfSvc: trust.RequesterAssociate,
			cp: policytest.Bidder, cpOp: policytest.BidderOperator, cpSvc: trust.BidderAssociate}
	}
	return parties{self: policytest.Bidder, selfOp: policytest.BidderOperator, selfSvc: trust.BidderAssociate,
		cp: policytest.Requester, cpOp: policytest.ReqOperator, cpSvc: trust.RequesterAssociate}
}

type trustCase struct {
	name  string
	build func(w *policytest.World, p parties)
	vouch bool
	only  bool
	mutal bool
}

var trustCases = []trustCase{
	{name: "unvouched", build: func(w *policytest.World, p parties) {}},
	{name: "vouched-by-self", vouch: true, build: func(w *policytest.World, p parties) {
		w.Vouch(p.self, p.cp)
	}},
	{name: "vouched-by-verified-self-operator", vouch: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		w.Vouch(p.selfOp, p.cp)
	}},
	{name: "vouched-by-self-claimed-self-operator", build: func(w *policytest.World, p parties) {
		w.Mirror(p.self, p.selfOp, p.selfSvc)
		w.Vouch(p.selfOp, p.cp)
	}},
	{name: "verified-counterparty-operator-vouched-by-self", vouch: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.cp, p.cpOp, p.cpSvc)
		w.Vouch(p.self, p.cpOp)
	}},
	{name: "verified-counterparty-operator-vouched-by-self-operator", vouch: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		w.Associate(p.cp, p.cpOp, p.cpSvc)
		w.Vouch(p.selfOp, p.cpOp)
	}},
	{name: "forged-counterparty-operator-claim", build: func(w *policytest.World, p parties) {
		w.Vouch(p.self, p.cpOp)
		w.Mirror(p.cp, p.cpOp, p.cpSvc)
	}},
	{name: "claim-for-the-other-role", build: func(w *policytest.World, p parties) {
		w.Vouch(p.self, p.cpOp)
		w.Associate(p.cp, p.cpOp, p.selfSvc)
	}},
	{name: "vouched-directly-with-verified-operator", vouch: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.cp, p.cpOp, p.cpSvc)
		w.Vouch(p.self, p.cp)
	}},
	{name: "counterparty-operator-vouches-self-only", build: func(w *policytest.World, p parties) {
		w.Associate(p.cp, p.cpOp, p.cpSvc)
		w.Vouch(p.cpOp, p.self)
	}},
	{name: "mutual-operator-vouches", vouch: true, mutal: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.cp, p.cpOp, p.cpSvc)
		w.Vouch(p.self, p.cpOp)
		w.Vouch(p.cpOp, p.self)
	}},
	{name: "denounced", build: func(w *policytest.World, p parties) {
		w.Denounce(p.self, p.cp)
	}},
	{name: "stranger-vouches", build: func(w *policytest.World, p parties) {
		w.Vouch(policytest.Stranger, p.cp)
		w.Vouch(p.cp, p.self)
	}},
	{name: "same-verified-operator", only: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		w.Associate(p.cp, p.selfOp, p.cpSvc)
	}},
	{name: "counterparty-operated-by-self", only: true, vouch: true, mutal: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.cp, p.self, p.cpSvc)
	}},
	{name: "forged-same-operator", build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		w.Mirror(p.cp, p.selfOp, p.cpSvc)
	}},
	{name: "different-verified-operators", build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		w.Associate(p.cp, p.cpOp, p.cpSvc)
	}},
	{name: "contract-scoped-association-held", only: true, build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		contract := w.HeldContract(p.selfOp, policytest.OtherBidder)
		w.ContractAssociation(p.cp, p.selfOp, p.cpSvc, contract, time.Now().Add(time.Hour))
	}},
	{name: "contract-scoped-association-expired", build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		contract := w.HeldContract(p.selfOp, policytest.OtherBidder)
		w.ContractAssociation(p.cp, p.selfOp, p.cpSvc, contract, time.Now().Add(-time.Minute))
	}},
	{name: "contract-scoped-association-released", build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		contract := w.HeldContract(p.selfOp, policytest.OtherBidder)
		w.ContractAssociation(p.cp, p.selfOp, p.cpSvc, contract, time.Now().Add(time.Hour))
		w.Release(p.selfOp, contract)
	}},
	{name: "contract-scoped-association-terminated", build: func(w *policytest.World, p parties) {
		w.Associate(p.self, p.selfOp, p.selfSvc)
		contract := w.HeldContract(p.selfOp, policytest.OtherBidder)
		w.ContractAssociation(p.cp, p.selfOp, p.cpSvc, contract, time.Now().Add(time.Hour))
		w.Terminate(contract)
	}},
}

type trustPolicy struct {
	family string
	want   func(trustCase) bool
	deny   string
}

var trustPolicies = []trustPolicy{
	{family: "tangled-vouch", want: func(c trustCase) bool { return c.vouch }},
	{family: "only-me", want: func(c trustCase) bool { return c.only }},
	{family: "mutuals", want: func(c trustCase) bool { return c.mutal }},
	{family: "open", want: func(trustCase) bool { return true }},
}

func workflowOf(family string, perspective side) string {
	if family == "open" {
		return "open"
	}
	return string(perspective) + "-" + family
}

func TestTrustPolicyMatrix(t *testing.T) {
	t.Parallel()
	for _, policy := range trustPolicies {
		for _, perspective := range []side{bidderSide, requesterSide} {
			for _, mode := range []lane{scope, full} {
				for _, tc := range trustCases {
					workflow := workflowOf(policy.family, perspective)
					t.Run("matrix/"+workflow+"/"+string(mode)+"/"+string(perspective)+"/"+tc.name, func(t *testing.T) {
						t.Parallel()
						h := policytest.New(t)
						p := partiesFor(perspective)
						tc.build(h.World, p)
						got := evaluate(t, h, workflow, perspective, mode, p.self, p.cp, nil)
						ran := got.mustRun(t, workflow)
						want := policy.want(tc)
						actionRan(t, ran, workflow, want)
						if want {
							got.mustAllow(t)
						} else {
							got.mustDeny(t, "")
						}
					})
				}
			}
		}
	}
}

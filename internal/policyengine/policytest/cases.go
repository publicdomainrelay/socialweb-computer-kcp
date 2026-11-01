package policytest

import (
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type Parties struct {
	Self, SelfOp, SelfSvc string
	CP, CPOp, CPSvc       string
}

func PartiesFor(perspective string) Parties {
	if perspective == "requester" {
		return Parties{Self: Requester, SelfOp: ReqOperator, SelfSvc: trust.RequesterAssociate,
			CP: Bidder, CPOp: BidderOperator, CPSvc: trust.BidderAssociate}
	}
	return Parties{Self: Bidder, SelfOp: BidderOperator, SelfSvc: trust.BidderAssociate,
		CP: Requester, CPOp: ReqOperator, CPSvc: trust.RequesterAssociate}
}

type TrustCase struct {
	Name    string
	Build   func(w *World, p Parties)
	Vouch   bool
	OnlyMe  bool
	Mutuals bool
}

var TrustCases = []TrustCase{
	{Name: "unvouched", Build: func(w *World, p Parties) {}},
	{Name: "vouched-by-self", Vouch: true, Build: func(w *World, p Parties) {
		w.Vouch(p.Self, p.CP)
	}},
	{Name: "vouched-by-verified-self-operator", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		w.Vouch(p.SelfOp, p.CP)
	}},
	{Name: "vouched-by-self-claimed-self-operator", Build: func(w *World, p Parties) {
		w.Mirror(p.Self, p.SelfOp, p.SelfSvc)
		w.Vouch(p.SelfOp, p.CP)
	}},
	{Name: "verified-counterparty-operator-vouched-by-self", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp, p.CPSvc)
		w.Vouch(p.Self, p.CPOp)
	}},
	{Name: "verified-counterparty-operator-vouched-by-self-operator", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		w.Associate(p.CP, p.CPOp, p.CPSvc)
		w.Vouch(p.SelfOp, p.CPOp)
	}},
	{Name: "forged-counterparty-operator-claim", Build: func(w *World, p Parties) {
		w.Vouch(p.Self, p.CPOp)
		w.Mirror(p.CP, p.CPOp, p.CPSvc)
	}},
	{Name: "claim-for-the-other-role", Build: func(w *World, p Parties) {
		w.Vouch(p.Self, p.CPOp)
		w.Associate(p.CP, p.CPOp, p.SelfSvc)
	}},
	{Name: "vouched-directly-with-verified-operator", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp, p.CPSvc)
		w.Vouch(p.Self, p.CP)
	}},
	{Name: "counterparty-operator-vouches-self-only", Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp, p.CPSvc)
		w.Vouch(p.CPOp, p.Self)
	}},
	{Name: "mutual-operator-vouches", Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp, p.CPSvc)
		w.Vouch(p.Self, p.CPOp)
		w.Vouch(p.CPOp, p.Self)
	}},
	{Name: "denounced", Build: func(w *World, p Parties) {
		w.Denounce(p.Self, p.CP)
	}},
	{Name: "stranger-vouches", Build: func(w *World, p Parties) {
		w.Vouch(Stranger, p.CP)
		w.Vouch(p.CP, p.Self)
	}},
	{Name: "same-verified-operator", OnlyMe: true, Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		w.Associate(p.CP, p.SelfOp, p.CPSvc)
	}},
	{Name: "counterparty-operated-by-self", OnlyMe: true, Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.Self, p.CPSvc)
	}},
	{Name: "forged-same-operator", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		w.Mirror(p.CP, p.SelfOp, p.CPSvc)
	}},
	{Name: "different-verified-operators", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		w.Associate(p.CP, p.CPOp, p.CPSvc)
	}},
	{Name: "contract-scoped-association-held", OnlyMe: true, Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, p.CPSvc, contract, time.Now().Add(time.Hour))
	}},
	{Name: "contract-scoped-association-expired", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, p.CPSvc, contract, time.Now().Add(-time.Minute))
	}},
	{Name: "contract-scoped-association-released", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, p.CPSvc, contract, time.Now().Add(time.Hour))
		w.Release(p.SelfOp, contract)
	}},
	{Name: "contract-scoped-association-terminated", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp, p.SelfSvc)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, p.CPSvc, contract, time.Now().Add(time.Hour))
		w.Terminate(contract)
	}},
}

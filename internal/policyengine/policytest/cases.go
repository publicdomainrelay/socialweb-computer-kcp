package policytest

import (
	"time"
)

type Parties struct {
	Self, SelfOp string
	CP, CPOp     string
}

func PartiesFor(perspective string) Parties {
	if perspective == "requester" {
		return Parties{Self: Requester, SelfOp: ReqOperator, CP: Bidder, CPOp: BidderOperator}
	}
	return Parties{Self: Bidder, SelfOp: BidderOperator, CP: Requester, CPOp: ReqOperator}
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
		w.Associate(p.Self, p.SelfOp)
		w.Vouch(p.SelfOp, p.CP)
	}},
	{Name: "vouched-by-self-declared-self-operator", Build: func(w *World, p Parties) {
		w.DeclareAgentOf(p.Self, p.SelfOp)
		w.Vouch(p.SelfOp, p.CP)
	}},
	{Name: "verified-counterparty-operator-vouched-by-self", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp)
		w.Vouch(p.Self, p.CPOp)
	}},
	{Name: "verified-counterparty-operator-vouched-by-self-operator", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		w.Associate(p.CP, p.CPOp)
		w.Vouch(p.SelfOp, p.CPOp)
	}},
	{Name: "forged-counterparty-operator", Build: func(w *World, p Parties) {
		w.Vouch(p.Self, p.CPOp)
		w.DeclareAgentOf(p.CP, p.CPOp)
	}},
	{Name: "pair-without-grant", Build: func(w *World, p Parties) {
		w.Vouch(p.Self, p.CPOp)
		w.DeclareAgentOf(p.CP, p.CPOp)
		w.Endorse(p.CPOp, p.CP)
	}},
	{Name: "vouched-directly-with-verified-operator", Vouch: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp)
		w.Vouch(p.Self, p.CP)
	}},
	{Name: "counterparty-operator-vouches-self-only", Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp)
		w.Vouch(p.CPOp, p.Self)
	}},
	{Name: "mutual-operator-vouches", Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.CPOp)
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
		w.Associate(p.Self, p.SelfOp)
		w.Associate(p.CP, p.SelfOp)
	}},
	{Name: "counterparty-operated-by-self", OnlyMe: true, Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.CP, p.Self)
	}},
	{Name: "forged-same-operator", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		w.DeclareAgentOf(p.CP, p.SelfOp)
	}},
	{Name: "different-verified-operators", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		w.Associate(p.CP, p.CPOp)
	}},
	{Name: "contract-scoped-association-held", OnlyMe: true, Vouch: true, Mutuals: true, Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, contract, time.Now().Add(time.Hour))
	}},
	{Name: "contract-scoped-association-expired", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, contract, time.Now().Add(-time.Minute))
	}},
	{Name: "contract-scoped-association-released", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, contract, time.Now().Add(time.Hour))
		w.Release(p.SelfOp, contract)
	}},
	{Name: "contract-scoped-association-terminated", Build: func(w *World, p Parties) {
		w.Associate(p.Self, p.SelfOp)
		contract := w.HeldContract(p.SelfOp, OtherBidder)
		w.ContractAssociation(p.CP, p.SelfOp, contract, time.Now().Add(time.Hour))
		w.Terminate(contract)
	}},
}

package policytest

import (
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const (
	Bidder         = "did:plc:policytestbidder"
	BidderOperator = "did:plc:policytestbidop"
	Requester      = "did:plc:policytestrequester"
	ReqOperator    = "did:plc:policytestreqop"
	Stranger       = "did:plc:policyteststranger"
	Forger         = "did:plc:policytestforger"
	OtherBidder    = "did:plc:policytestbidder2"
	createdAt      = "2026-10-30T00:00:00Z"
)

type Ref struct {
	URI string
	CID string
}

func (w *World) put(repo, collection, rkey string, value map[string]any) Ref {
	uri := URI(repo, collection, rkey)
	return Ref{URI: uri, CID: w.Put(uri, value)}
}

func (w *World) Vouch(voucher, subject string) Ref {
	return w.put(voucher, trust.VouchNSID, subject, map[string]any{"$type": trust.VouchNSID, "subject": subject, "createdAt": createdAt})
}

func (w *World) Denounce(voucher, subject string) Ref {
	return w.put(voucher, trust.VouchNSID, subject, map[string]any{"$type": trust.VouchNSID, "subject": subject, "kind": "denounce",
		"createdAt": createdAt})
}

func (w *World) Unvouch(voucher, subject string) {
	w.Delete(URI(voucher, trust.VouchNSID, subject))
}

func (w *World) DeclareAgentOf(agent, principal string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.docs[agent] = append(w.docs[agent], identlink.AgentOfPrefix+principal)
}

func (w *World) Endorse(principal, agent string) Ref {
	return w.put(principal, identlink.EndorsementNSID, agent, map[string]any{"$type": identlink.EndorsementNSID, "agent": agent})
}

func (w *World) Grant(principal, agent string) Ref {
	return w.put(principal, identlink.GrantNSID, agent, map[string]any{"$type": identlink.GrantNSID, "agent": agent, "createdAt": createdAt})
}

func (w *World) Associate(agent, principal string) {
	w.DeclareAgentOf(agent, principal)
	w.Endorse(principal, agent)
	w.Grant(principal, agent)
}

type Contract struct {
	Accept  Ref
	Receipt Ref
	Bidder  string
}

func (w *World) HeldContract(operator, bidder string) Contract {
	bid := w.put(bidder, market.BidNSID, "policytest-bid", map[string]any{"$type": market.BidNSID, "createdAt": createdAt})
	accept := w.put(operator, market.AcceptNSID, "policytest-accept", map[string]any{"$type": market.AcceptNSID,
		"bid": map[string]any{"uri": bid.URI, "cid": bid.CID}, "createdAt": createdAt})
	receipt := w.put(bidder, market.ReceiptNSID, market.ReceiptRKey(accept.URI, accept.CID), map[string]any{"$type": market.ReceiptNSID,
		"accept": map[string]any{"uri": accept.URI, "cid": accept.CID}, "createdAt": createdAt})
	return Contract{Accept: accept, Receipt: receipt, Bidder: bidder}
}

func (w *World) ContractAssociation(agent, principal string, contract Contract, expiresAt time.Time) Ref {
	w.DeclareAgentOf(agent, principal)
	w.Endorse(principal, agent)
	return w.put(principal, identlink.GrantNSID, agent, map[string]any{
		"$type":     identlink.GrantNSID,
		"agent":     agent,
		"contract":  map[string]any{"uri": contract.Accept.URI, "cid": contract.Accept.CID},
		"expiresAt": expiresAt.UTC().Format(time.RFC3339Nano),
		"createdAt": createdAt,
	})
}

func (w *World) Release(operator string, contract Contract) {
	w.put(operator, market.ComputeEventsVMDeleteNSID, market.ReleasePayloadRKey(contract.Receipt.URI),
		map[string]any{"$type": market.ComputeEventsVMDeleteNSID, "createdAt": createdAt})
}

func (w *World) Terminate(contract Contract) {
	rkey, _ := market.EventRKeys(market.TerminationKey(contract.Receipt.URI))
	w.put(contract.Bidder, market.ComputeEventsVMTerminatedNSID, rkey,
		map[string]any{"$type": market.ComputeEventsVMTerminatedNSID, "createdAt": createdAt})
}

func (w *World) PolicyRecord(repo, rkey, name, workflowText string) Ref {
	return w.put(repo, PolicyGhaLiteNSID, rkey, map[string]any{"$type": PolicyGhaLiteNSID, "name": name, "workflow": workflowText,
		"createdAt": createdAt})
}

func (w *World) RecordOfType(repo, collection, rkey string, value map[string]any) Ref {
	return w.put(repo, collection, rkey, value)
}

func (w *World) RFP(repo, rkey string, policies ...Ref) Ref {
	attached := make([]any, 0, len(policies))
	for _, policy := range policies {
		attached = append(attached, map[string]any{"uri": policy.URI, "cid": policy.CID})
	}
	return w.put(repo, market.RFPNSID, rkey, map[string]any{"$type": market.RFPNSID, "policies": attached, "createdAt": createdAt})
}

func (w *World) Bid(bidder, rkey string, free bool, cost float64) Ref {
	payloadNSID := market.BidsX402NSID
	payloadValue := map[string]any{"$type": payloadNSID, "cost": cost, "createdAt": createdAt}
	if free {
		payloadNSID = market.BidsFreeNSID
		payloadValue = map[string]any{"$type": payloadNSID, "reason": "free", "createdAt": createdAt}
	}
	payload := w.put(bidder, payloadNSID, rkey+"-payload", payloadValue)
	return w.put(bidder, market.BidNSID, rkey, map[string]any{"$type": market.BidNSID,
		"payload": map[string]any{"uri": payload.URI, "cid": payload.CID}, "createdAt": createdAt})
}

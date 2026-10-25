package market

import (
	"cmp"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type Addressees struct {
	DIDs map[string]bool

	OpenRFPs map[string]bool

	ClosedRFPs map[string]bool

	Accepts map[string]bool

	Receipts map[string]bool

	Counterparts map[string]bool

	Awaiting map[string]bool

	Collecting bool
}

func newAddressees(dids []string) *Addressees {
	a := &Addressees{DIDs: map[string]bool{}, OpenRFPs: map[string]bool{}, ClosedRFPs: map[string]bool{}, Accepts: map[string]bool{},
		Receipts: map[string]bool{}, Counterparts: map[string]bool{}, Awaiting: map[string]bool{}}
	for _, did := range dids {
		a.addDID(did)
	}
	return a
}

func (a *Addressees) addDID(did string) {
	if did = strings.TrimSpace(did); strings.HasPrefix(did, "did:") {
		a.DIDs[did] = true
	}
}

func (a *Addressees) addURI(set map[string]bool, uri string) {
	if uri = strings.TrimSpace(uri); uri != "" {
		set[uri] = true
	}
}

func RequesterContractDone(contract v1alpha1.ComputeContract) bool {
	state := contract.Status.Contract
	phase := cmp.Or(contract.Status.Phase, v1alpha1.ComputeContractPending)
	terminal := phase == v1alpha1.ComputeContractReleased
	if phase == v1alpha1.ComputeContractFailed {
		terminal = state == nil || state.Receipt == nil
	}
	if terminal && state != nil && state.Receipt != nil && state.Termination == nil && state.Winner != nil && state.Winner.DID != "" {
		terminal = false
	}
	return terminal
}

func RequesterAddresseesOf(contracts []v1alpha1.ComputeContract, live bool, dids ...string) *Addressees {
	a := newAddressees(dids)
	for _, contract := range contracts {
		done := RequesterContractDone(contract)
		state := contract.Status.Contract
		if state == nil {
			if !done && contract.DeletionTimestamp == nil {
				a.Collecting = true
			}
			continue
		}
		rfp := state.RFPURI
		if state.RFP != nil {
			rfp = cmp.Or(state.RFP.URI, rfp)
		}
		if rfp != "" {
			a.addDID(ATURIAuthority(rfp))
		}
		a.addDID(state.RequesterDid)
		a.addDID(state.MarketDid)
		accept := state.AcceptURI
		if state.Accept != nil {
			accept = cmp.Or(state.Accept.URI, accept)
		}
		receipt := state.ReceiptURI
		if state.Receipt != nil {
			receipt = cmp.Or(state.Receipt.URI, receipt)
		}
		accepted := accept != "" || receipt != ""
		if live && done {
			a.addURI(a.ClosedRFPs, rfp)
			continue
		}
		if accepted || done {
			a.addURI(a.ClosedRFPs, rfp)
		} else {
			a.Collecting = true
			a.addURI(a.OpenRFPs, rfp)
		}
		a.addURI(a.Accepts, accept)
		a.addURI(a.Receipts, receipt)
		winner := ""
		if state.Winner != nil {
			winner = state.Winner.DID
		}
		if winner == "" && contract.Status.Chosen != nil {
			winner = contract.Status.Chosen.Did
		}
		if winner == "" && state.Bid != nil {
			winner = ATURIAuthority(state.Bid.URI)
		}
		for _, bid := range contract.Status.Bids {
			if bid.Did != "" {
				a.Counterparts[bid.Did] = true
			}
		}
		if winner != "" {
			a.Counterparts[winner] = true
			if receipt == "" {
				a.Awaiting[winner] = true
			}
		}
	}
	return a
}

func BidderAddresseesOf(dids ...string) *Addressees {
	return newAddressees(dids)
}

func (a *Addressees) own(did string) bool {
	return did != "" && a.DIDs[did]
}

func RequesterConsumes(collection string) bool {
	switch collection {
	case BidNSID, ReceiptNSID, EventNSID:
		return true
	}
	return false
}

func BidderConsumes(collection string) bool {
	switch collection {
	case RFPNSID, AcceptNSID, EventNSID:
		return true
	}
	return false
}

func (a *Addressees) RequesterWants(collection, author string) bool {
	switch collection {
	case BidNSID:
		return a.Collecting && !a.own(author)
	case ReceiptNSID, EventNSID:
		return a.Counterparts[author] && !a.own(author)
	}
	return false
}

func (a *Addressees) RequesterAddressed(collection, author string, record map[string]any) bool {
	switch collection {
	case BidNSID:
		bid, err := ParseBid(record)
		if err != nil {
			return false
		}
		rfp := bid.RFP.URI
		switch {
		case a.OpenRFPs[rfp]:
			return true
		case a.ClosedRFPs[rfp]:
			return false
		}
		return a.Collecting && a.own(ATURIAuthority(rfp))
	case ReceiptNSID:
		receipt, err := ParseReceipt(record)
		if err != nil {
			return false
		}
		return a.Accepts[receipt.Accept.URI] || (a.Awaiting[author] && a.own(ATURIAuthority(receipt.Accept.URI)))
	case EventNSID:
		event, err := ParseEvent(record)
		if err != nil {
			return false
		}
		return a.Receipts[event.Receipt.URI] || (a.Awaiting[author] && ATURIAuthority(event.Receipt.URI) == author)
	}
	return false
}

func (a *Addressees) BidderAddressed(collection string, record map[string]any) bool {
	switch collection {
	case RFPNSID:
		return true
	case AcceptNSID:
		accept, err := ParseAccept(record)
		if err != nil {
			return false
		}
		return len(a.DIDs) == 0 || a.own(ATURIAuthority(accept.Bid.URI))
	case EventNSID:
		event, err := ParseEvent(record)
		if err != nil {
			return false
		}
		return len(a.DIDs) == 0 || a.own(ATURIAuthority(event.Receipt.URI))
	}
	return false
}

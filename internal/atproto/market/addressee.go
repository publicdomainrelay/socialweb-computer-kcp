package market

import (
	"cmp"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type ContractRef struct {
	Name string

	UID string
}

type Addressees struct {
	DIDs map[string]bool

	RFPs map[string]ContractRef

	Accepts map[string]ContractRef

	Receipts map[string]ContractRef

	Winners map[string]bool

	Awaiting map[string]bool
}

func newAddressees() *Addressees {
	return &Addressees{DIDs: map[string]bool{}, RFPs: map[string]ContractRef{}, Accepts: map[string]ContractRef{},
		Receipts: map[string]ContractRef{}, Winners: map[string]bool{}, Awaiting: map[string]bool{}}
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

type ContractRefs struct {
	RFP string

	Accept string

	Receipt string

	Winner string
}

func RefsOfContract(state *v1alpha1.NodeClaimContract) ContractRefs {
	var refs ContractRefs
	if state == nil {
		return refs
	}
	refs.RFP = state.RFPURI
	if state.RFP != nil {
		refs.RFP = cmp.Or(state.RFP.URI, refs.RFP)
	}
	refs.Accept = state.AcceptURI
	if state.Accept != nil {
		refs.Accept = cmp.Or(state.Accept.URI, refs.Accept)
	}
	refs.Receipt = state.ReceiptURI
	if state.Receipt != nil {
		refs.Receipt = cmp.Or(state.Receipt.URI, refs.Receipt)
	}
	if state.Bid != nil {
		refs.Winner = ATURIAuthority(state.Bid.URI)
	}
	if refs.Winner == "" && state.Winner != nil {
		refs.Winner = state.Winner.DID
	}
	return refs
}

func (a *Addressees) Add(ref ContractRef, refs ContractRefs, open bool) {
	if refs.RFP != "" && open {
		a.RFPs[refs.RFP] = ref
	}
	if refs.Accept != "" {
		a.Accepts[refs.Accept] = ref
		if refs.Winner != "" {
			a.Winners[refs.Winner] = true
			if refs.Receipt == "" {
				a.Awaiting[refs.Winner] = true
			}
		}
	}
	if refs.Receipt != "" {
		a.Receipts[refs.Receipt] = ref
	}
}

func RequesterContractSettled(contract v1alpha1.ComputeContract) bool {
	if RequesterContractDone(contract) {
		return true
	}
	return contract.Status.Phase == v1alpha1.ComputeContractReleased &&
		apimeta.IsStatusConditionTrue(contract.Status.Conditions, v1alpha1.ComputeContractConditionReleaseAcknowledged)
}

func RequesterContractEnded(contract v1alpha1.ComputeContract) bool {
	return contract.Status.Phase == v1alpha1.ComputeContractReleased || contract.Status.Phase == v1alpha1.ComputeContractFailed
}

func RequesterAddresseesOf(contracts []v1alpha1.ComputeContract, skip func(v1alpha1.ComputeContract) bool) *Addressees {
	a := newAddressees()
	for _, contract := range contracts {
		if skip != nil && skip(contract) {
			continue
		}
		refs := RefsOfContract(contract.Status.Contract)
		a.Add(ContractRef{Name: contract.Name, UID: string(contract.UID)}, refs, skip == nil || refs.Accept == "")
	}
	return a
}

func BidderAddresseesOf(dids ...string) *Addressees {
	a := newAddressees()
	for _, did := range dids {
		if did = strings.TrimSpace(did); strings.HasPrefix(did, "did:") {
			a.DIDs[did] = true
		}
	}
	return a
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
		return len(a.RFPs) > 0
	case ReceiptNSID, EventNSID:
		return a.Winners[author]
	}
	return false
}

func (a *Addressees) RequesterMatch(collection, author string, record map[string]any) (ContractRef, string, bool) {
	switch collection {
	case BidNSID:
		bid, err := ParseBid(record)
		if err != nil {
			return ContractRef{}, "", false
		}
		ref, ok := a.RFPs[bid.RFP.URI]
		return ref, "", ok
	case ReceiptNSID:
		receipt, err := ParseReceipt(record)
		if err != nil {
			return ContractRef{}, "", false
		}
		ref, ok := a.Accepts[receipt.Accept.URI]
		return ref, "", ok
	case EventNSID:
		event, err := ParseEvent(record)
		if err != nil {
			return ContractRef{}, "", false
		}
		if ref, ok := a.Receipts[event.Receipt.URI]; ok {
			return ref, "", true
		}
		if a.Awaiting[author] && ATURIAuthority(event.Receipt.URI) == author && NSIDFromURI(event.Receipt.URI) == ReceiptNSID {
			return ContractRef{}, event.Receipt.URI, false
		}
	}
	return ContractRef{}, "", false
}

func (a *Addressees) ReceiptMatch(record map[string]any) (ContractRef, bool) {
	receipt, err := ParseReceipt(record)
	if err != nil {
		return ContractRef{}, false
	}
	ref, ok := a.Accepts[receipt.Accept.URI]
	return ref, ok
}

func (a *Addressees) own(did string) bool {
	return did != "" && a.DIDs[did]
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

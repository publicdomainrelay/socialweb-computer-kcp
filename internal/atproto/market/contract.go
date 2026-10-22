package market

import (
	"context"
	"fmt"
)

type ContractGraph struct {
	Bid        ResolvedRecord
	RFP        ResolvedRecord
	RFPPayload ResolvedRecord
	BidPayload ResolvedRecord
	BidConfig  *ResolvedRecord
}

type ContractVerifyOptions struct {
	SkipSignatureVerification bool
	KeysForDID                KeysForDID
	KeyResolver               KeyResolver
	KeyBoundByBadgeBlueKey    func(ctx context.Context, did, key string) (bool, error)
}

func ResolveContractGraph(ctx context.Context, accept Accept, resolver RecordResolver, opts ContractVerifyOptions) (*ContractGraph, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: no record resolver", ErrContractGraph)
	}
	bidRef := RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID}
	bidRecord, err := resolver.Resolve(ctx, bidRef)
	if err != nil {
		return nil, &ContractGraphError{Reason: "resolve bid: " + err.Error()}
	}
	bid, err := ParseBid(bidRecord.Value)
	if err != nil {
		return nil, &ContractGraphError{Reason: "parse bid: " + err.Error()}
	}
	if !RefsEqual(RecordRef{URI: bid.RFP.URI, CID: bid.RFP.CID}, RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID}) {
		return nil, &ContractGraphError{Reason: "Accept.rfp does not match Bid.rfp"}
	}
	rfpRecord, err := resolver.Resolve(ctx, RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID})
	if err != nil {
		return nil, &ContractGraphError{Reason: "resolve rfp: " + err.Error()}
	}
	rfp, err := ParseRFP(rfpRecord.Value)
	if err != nil {
		return nil, &ContractGraphError{Reason: "parse rfp: " + err.Error()}
	}
	if !opts.SkipSignatureVerification {
		verifyOpts := VerifyRecordOptions{
			KeysForDID:             opts.KeysForDID,
			KeyResolver:            opts.KeyResolver,
			KeyBoundByBadgeBlueKey: opts.KeyBoundByBadgeBlueKey,
		}
		if !VerifyRecordSignatures(ctx, bidRecord.Value, ATURIAuthority(bidRecord.URI), verifyOpts) {
			return nil, &ContractGraphError{Reason: "Bid is missing a valid badge.blue signature"}
		}
		if !VerifyRecordSignatures(ctx, rfpRecord.Value, ATURIAuthority(rfpRecord.URI), verifyOpts) {
			return nil, &ContractGraphError{Reason: "RFP is missing a valid badge.blue signature"}
		}
	}
	rfpPayload, err := resolver.Resolve(ctx, RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return nil, &ContractGraphError{Reason: "resolve rfp payload: " + err.Error()}
	}
	bidPayload, err := resolver.Resolve(ctx, RecordRef{URI: bid.Payload.URI, CID: bid.Payload.CID})
	if err != nil {
		return nil, &ContractGraphError{Reason: "resolve bid payload: " + err.Error()}
	}
	var bidConfig *ResolvedRecord
	if bid.Config != nil {
		cfg, err := resolver.Resolve(ctx, RecordRef{URI: bid.Config.URI, CID: bid.Config.CID})
		if err != nil {
			return nil, &ContractGraphError{Reason: "resolve bid config: " + err.Error()}
		}
		bidConfig = &cfg
	}
	return &ContractGraph{
		Bid:        bidRecord,
		RFP:        rfpRecord,
		RFPPayload: rfpPayload,
		BidPayload: bidPayload,
		BidConfig:  bidConfig,
	}, nil
}

type ReceiptVerification struct {
	OK     bool
	SigOK  bool
	BindOK bool
}

func VerifyReceipt(ctx context.Context, receiptRecord map[string]any, receiptURI string, acceptRecord map[string]any, acceptURI string, opts VerifyRecordOptions) ReceiptVerification {
	opts.KeysForDID = nil
	sigOK := VerifyRecordSignatures(ctx, receiptRecord, ATURIAuthority(receiptURI), opts)
	bindOK := VerifyRemoteProof(acceptRecord, ATURIAuthority(acceptURI), receiptRecord)
	return ReceiptVerification{OK: sigOK && bindOK, SigOK: sigOK, BindOK: bindOK}
}

func BindEventPayload(event Event, eventURI, receiptURI, payloadNSID string) (RecordRef, error) {
	if event.Type != EventNSID {
		return RecordRef{}, fmt.Errorf("%w: envelope is not a market.event", ErrInvalidEvent)
	}
	if event.Receipt.URI != receiptURI {
		return RecordRef{}, fmt.Errorf("%w: event names another receipt", ErrInvalidEvent)
	}
	bidder := ATURIAuthority(receiptURI)
	if ATURIAuthority(eventURI) != bidder {
		return RecordRef{}, fmt.Errorf("%w: event not authored by the receipt's bidder", ErrInvalidEvent)
	}
	payload := RecordRef{URI: event.Payload.URI, CID: event.Payload.CID}
	if ATURIAuthority(payload.URI) != bidder {
		return RecordRef{}, fmt.Errorf("%w: payload not in the bidder's repo", ErrInvalidEvent)
	}
	if NSIDFromURI(payload.URI) != payloadNSID {
		return RecordRef{}, fmt.Errorf("%w: payload is not %s", ErrInvalidEvent, payloadNSID)
	}
	return payload, nil
}

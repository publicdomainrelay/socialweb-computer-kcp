package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

func validateSignatures(sigs Signatures) error {
	if len(sigs) == 0 {
		return fmt.Errorf("%w: missing signatures", ErrInvalidRecord)
	}
	for _, sig := range sigs {
		if err := sig.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s Signature) validate() error {
	switch {
	case s.AttestedSignature != nil:
		return validateInlineSignature(s.AttestedSignature.Key, true, s.AttestedSignature.CID)
	case s.StrongRef != nil:
		_, err := ParseRecordRef(s.StrongRef.URI, s.StrongRef.CID)
		return err
	}
	var entry struct {
		Key       *string         `json:"key"`
		Signature json.RawMessage `json:"signature"`
		URI       *string         `json:"uri"`
		CID       *string         `json:"cid"`
	}
	if err := json.Unmarshal(s.Unknown, &entry); err != nil {
		return fmt.Errorf("%w: signature entry is not an object", ErrInvalidRecord)
	}
	if entry.Key != nil {
		var raw any
		_ = json.Unmarshal(entry.Signature, &raw)
		_, ok := normalizeSignatureBytes(raw)
		return validateInlineSignature(*entry.Key, ok, deref(entry.CID))
	}
	if entry.URI == nil || entry.CID == nil {
		return fmt.Errorf("%w: signature entry is neither inline nor a strongRef", ErrInvalidRecord)
	}
	_, err := ParseRecordRef(*entry.URI, *entry.CID)
	return err
}

func validateInlineSignature(key string, hasBytes bool, cidStr string) error {
	if key == "" {
		return fmt.Errorf("%w: signature missing key", ErrInvalidRecord)
	}
	if !hasBytes {
		return fmt.Errorf("%w: signature missing signature bytes", ErrInvalidRecord)
	}
	if cidStr == "" || !IsAttestationCIDString(cidStr) {
		return fmt.Errorf("%w: signature missing valid cid", ErrInvalidRecord)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func validateOptionalDatetime(value, field string) error {
	if value == "" {
		return nil
	}
	if _, err := syntax.ParseDatetime(value); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidRecord, field, err)
	}
	return nil
}

func validateRefCollection(ref StrongRef, field, collection string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if NSIDFromURI(ref.URI) != collection {
		return fmt.Errorf("%w: %s is not a %s ref", ErrInvalidRecord, field, collection)
	}
	return nil
}

func validateOptionalRef(ref *StrongRef) error {
	if ref == nil {
		return nil
	}
	return ref.Validate()
}

func validateOptionalServiceRef(value string) error {
	if value == "" {
		return nil
	}
	return ValidServiceRef(value)
}

func validateAttestationCID(value, what string) error {
	if value == "" || !IsAttestationCIDString(value) {
		return fmt.Errorf("%w: %s cid is not an attestation cid", ErrInvalidRecord, what)
	}
	return nil
}

func validateRelayURLs(relays []string) error {
	if len(relays) == 0 {
		return fmt.Errorf("%w: relays is empty", ErrInvalidRecord)
	}
	for _, raw := range relays {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: relay url %q", ErrInvalidRecord, raw)
		}
	}
	return nil
}

func validateAppliesTo(appliesTo []string) error {
	if len(appliesTo) == 0 {
		return fmt.Errorf("%w: offering missing appliesTo", ErrInvalidRecord)
	}
	for _, nsid := range appliesTo {
		if _, err := syntax.ParseNSID(nsid); err != nil {
			return fmt.Errorf("%w: appliesTo: %v", ErrInvalidRecord, err)
		}
	}
	return nil
}

func validateRecordURI(value, what string) error {
	aturi, err := syntax.ParseATURI(value)
	if err != nil || aturi.Collection() == "" || aturi.RecordKey() == "" {
		return fmt.Errorf("%w: %s is not a record AT URI", ErrInvalidRecord, what)
	}
	return nil
}

func (r *RFP) Validate() error {
	if err := r.Payload.Validate(); err != nil {
		return err
	}
	for _, p := range r.Policies {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	if err := validateOptionalServiceRef(r.SubmitBid); err != nil {
		return err
	}
	if err := validateOptionalDatetime(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateSignatures(r.Signatures)
}

func (r *Bid) Validate() error {
	if err := validateRefCollection(r.RFP, "rfp", RFPNSID); err != nil {
		return err
	}
	if err := r.Payload.Validate(); err != nil {
		return err
	}
	if err := validateOptionalRef(r.Config); err != nil {
		return err
	}
	if err := validateOptionalServiceRef(r.SubmitAccept); err != nil {
		return err
	}
	if err := validateOptionalDatetime(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateSignatures(r.Signatures)
}

func (r *Accept) Validate() error {
	if err := validateRefCollection(r.RFP, "rfp", RFPNSID); err != nil {
		return err
	}
	if err := validateRefCollection(r.Bid, "bid", BidNSID); err != nil {
		return err
	}
	if err := validateOptionalRef(r.Payload); err != nil {
		return err
	}
	if err := validateOptionalServiceRef(r.SubmitEvent); err != nil {
		return err
	}
	if err := validateOptionalDatetime(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateSignatures(r.Signatures)
}

func (r *Receipt) Validate() error {
	if r.RFP.URI != "" || r.RFP.CID != "" {
		if err := validateRefCollection(r.RFP, "rfp", RFPNSID); err != nil {
			return err
		}
	}
	if r.Bid.URI != "" || r.Bid.CID != "" {
		if err := validateRefCollection(r.Bid, "bid", BidNSID); err != nil {
			return err
		}
	}
	if err := validateRefCollection(r.Accept, "accept", AcceptNSID); err != nil {
		return err
	}
	if err := validateOptionalRef(r.Payload); err != nil {
		return err
	}
	if err := validateOptionalServiceRef(r.SubmitEvent); err != nil {
		return err
	}
	if err := validateAttestationCID(r.CID, "receipt"); err != nil {
		return err
	}
	if err := validateOptionalDatetime(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateSignatures(r.Signatures)
}

func (r *Event) Validate() error {
	if err := validateRefCollection(r.Receipt, "receipt", ReceiptNSID); err != nil {
		return err
	}
	if err := r.Payload.Validate(); err != nil {
		return err
	}
	if err := validateOptionalDatetime(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateSignatures(r.Signatures)
}

func (r *Offering) Validate() error {
	r.EndpointURL = strings.TrimSpace(r.EndpointURL)
	if err := validateOptionalServiceRef(r.EndpointURL); err != nil {
		return err
	}
	if err := validateAppliesTo(r.AppliesTo); err != nil {
		return err
	}
	if r.CreatedAt == "" {
		return fmt.Errorf("%w: offering missing createdAt", ErrInvalidRecord)
	}
	if err := validateOptionalDatetime(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateOptionalDatetime(r.RefreshedAt, "refreshedAt")
}

func (r *Heartbeat) Validate() error {
	if err := validateRecordURI(r.Offering, "heartbeat offering"); err != nil {
		return err
	}
	if r.CreatedAt == "" {
		return fmt.Errorf("%w: heartbeat missing createdAt", ErrInvalidRecord)
	}
	return validateOptionalDatetime(r.CreatedAt, "createdAt")
}

func (r *Relays) Validate() error {
	if err := validateRelayURLs(r.Relays); err != nil {
		return err
	}
	if r.CreatedAt == "" {
		return fmt.Errorf("%w: relays missing createdAt", ErrInvalidRecord)
	}
	return validateOptionalDatetime(r.CreatedAt, "createdAt")
}

func (r *BadgeBlueKeys) Validate() error {
	if r.KeyID == "" || r.Challenge == "" || r.CreatedAt == "" {
		return fmt.Errorf("%w: badgeBlueKeys missing required fields", ErrInvalidRecord)
	}
	if _, err := syntax.ParseDID(r.Challenge); err != nil {
		return fmt.Errorf("%w: challenge: %v", ErrInvalidRecord, err)
	}
	return validateOptionalDatetime(r.CreatedAt, "createdAt")
}

func (r *BidsFree) Validate() error {
	return validateSignatures(r.Signatures)
}

func (r *BidsX402) Validate() error {
	if _, missing := r.absent["cost"]; missing {
		return fmt.Errorf("%w: bids.x402 missing cost", ErrInvalidRecord)
	}
	if r.Currency == "" || r.Frequency == "" || r.URL == "" {
		return fmt.Errorf("%w: bids.x402 missing required fields", ErrInvalidRecord)
	}
	return validateSignatures(r.Signatures)
}

func validateAccepts(bid StrongRef, payload *StrongRef, sigs Signatures) error {
	if err := validateRefCollection(bid, "bid", BidNSID); err != nil {
		return err
	}
	if err := validateOptionalRef(payload); err != nil {
		return err
	}
	return validateSignatures(sigs)
}

func (r *AcceptsFree) Validate() error {
	return validateAccepts(r.Bid, r.Payload, r.Signatures)
}

func (r *AcceptsX402) Validate() error {
	return validateAccepts(r.Bid, r.Payload, r.Signatures)
}

func validateReceipts(accept StrongRef, cidStr string, sigs Signatures) error {
	if err := validateRefCollection(accept, "accept", AcceptNSID); err != nil {
		return err
	}
	if err := validateAttestationCID(cidStr, "receipts"); err != nil {
		return err
	}
	return validateSignatures(sigs)
}

func (r *ReceiptsFree) Validate() error {
	return validateReceipts(r.Accept, r.CID, r.Signatures)
}

func (r *ReceiptsX402) Validate() error {
	return validateReceipts(r.Accept, r.CID, r.Signatures)
}

func strongRef(ref RecordRef) (StrongRef, error) {
	if _, err := ParseRecordRef(ref.URI, ref.CID); err != nil {
		return StrongRef{}, err
	}
	return NewStrongRef(ref.URI, ref.CID), nil
}

func optionalStrongRef(ref *RecordRef) (*StrongRef, error) {
	if ref == nil {
		return nil, nil
	}
	out, err := strongRef(*ref)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func isoOrNow(value string) string {
	if value == "" {
		return NowISO()
	}
	return value
}

func createSigned(ctx context.Context, w RecordWriter, repo, rkey string, record Record, signer *Signer) (SignedRecord, error) {
	value, err := EncodeRecord(record)
	if err != nil {
		return SignedRecord{}, err
	}
	if rkey == "" {
		return CreateSignedRecord(ctx, w, repo, record.NSID(), value, signer)
	}
	return CreateSignedRecordAt(ctx, w, repo, record.NSID(), rkey, value, signer)
}

type RFPOptions struct {
	Payload   RecordRef
	SubmitBid string
	Domain    string
	Policies  []RecordRef
	CreatedAt string
}

func BuildRFP(opts RFPOptions) (RFP, error) {
	payload, err := strongRef(opts.Payload)
	if err != nil {
		return RFP{}, err
	}
	if err := validateOptionalServiceRef(opts.SubmitBid); err != nil {
		return RFP{}, err
	}
	out := RFP{Payload: payload, Domain: opts.Domain, SubmitBid: opts.SubmitBid, CreatedAt: isoOrNow(opts.CreatedAt)}
	for _, p := range opts.Policies {
		ref, err := strongRef(p)
		if err != nil {
			return RFP{}, err
		}
		out.Policies = append(out.Policies, ref)
	}
	return out, nil
}

func CreateRFP(ctx context.Context, w RecordWriter, repo string, opts RFPOptions, signer *Signer) (SignedRecord, error) {
	return CreateRFPAt(ctx, w, repo, "", opts, signer)
}

func CreateRFPAt(ctx context.Context, w RecordWriter, repo, rkey string, opts RFPOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildRFP(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return createSigned(ctx, w, repo, rkey, record, signer)
}

type BidOptions struct {
	RFP          RecordRef
	Config       *RecordRef
	Payload      RecordRef
	SubmitAccept string
	CreatedAt    string
}

func BuildBid(opts BidOptions) (Bid, error) {
	rfp, err := strongRef(opts.RFP)
	if err != nil {
		return Bid{}, err
	}
	payload, err := strongRef(opts.Payload)
	if err != nil {
		return Bid{}, err
	}
	config, err := optionalStrongRef(opts.Config)
	if err != nil {
		return Bid{}, err
	}
	if err := validateOptionalServiceRef(opts.SubmitAccept); err != nil {
		return Bid{}, err
	}
	return Bid{RFP: rfp, Config: config, Payload: payload, SubmitAccept: opts.SubmitAccept, CreatedAt: isoOrNow(opts.CreatedAt)}, nil
}

func BidRKey(bidderDID, rfpURI, rfpCID string) string {
	sum := sha256.Sum256([]byte(bidderDID + "\x00" + rfpURI + "#" + rfpCID))
	return "bid-" + hex.EncodeToString(sum[:])[:20]
}

func CreateBid(ctx context.Context, w RecordWriter, repo string, opts BidOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildBid(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return createSigned(ctx, w, repo, "", record, signer)
}

type AcceptOptions struct {
	RFP         RecordRef
	Bid         RecordRef
	Payload     *RecordRef
	SubmitEvent string
	CreatedAt   string
}

func BuildAccept(opts AcceptOptions) (Accept, error) {
	rfp, err := strongRef(opts.RFP)
	if err != nil {
		return Accept{}, err
	}
	bid, err := strongRef(opts.Bid)
	if err != nil {
		return Accept{}, err
	}
	payload, err := optionalStrongRef(opts.Payload)
	if err != nil {
		return Accept{}, err
	}
	if err := validateOptionalServiceRef(opts.SubmitEvent); err != nil {
		return Accept{}, err
	}
	return Accept{RFP: rfp, Bid: bid, Payload: payload, SubmitEvent: opts.SubmitEvent, CreatedAt: isoOrNow(opts.CreatedAt)}, nil
}

func CreateAcceptAt(ctx context.Context, w RecordWriter, repo, rkey string, opts AcceptOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildAccept(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return createSigned(ctx, w, repo, rkey, record, signer)
}

type ReceiptRefs struct {
	RFP         RecordRef
	Bid         RecordRef
	Accept      RecordRef
	Payload     *RecordRef
	SubmitEvent string
}

func BuildReceipt(refs ReceiptRefs, createdAt string) (Receipt, error) {
	rfp, err := strongRef(refs.RFP)
	if err != nil {
		return Receipt{}, err
	}
	bid, err := strongRef(refs.Bid)
	if err != nil {
		return Receipt{}, err
	}
	accept, err := strongRef(refs.Accept)
	if err != nil {
		return Receipt{}, err
	}
	payload, err := optionalStrongRef(refs.Payload)
	if err != nil {
		return Receipt{}, err
	}
	if err := validateOptionalServiceRef(refs.SubmitEvent); err != nil {
		return Receipt{}, err
	}
	out := Receipt{RFP: rfp, Bid: bid, Accept: accept, Payload: payload, SubmitEvent: refs.SubmitEvent, CreatedAt: isoOrNow(createdAt)}
	if payload == nil {
		out.Extra = map[string]json.RawMessage{"payload": json.RawMessage("null")}
	}
	return out, nil
}

type EventOptions struct {
	Receipt   RecordRef
	Payload   RecordRef
	CreatedAt string
}

func BuildEvent(opts EventOptions) (Event, error) {
	receipt, err := strongRef(opts.Receipt)
	if err != nil {
		return Event{}, err
	}
	payload, err := strongRef(opts.Payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Receipt: receipt, Payload: payload, CreatedAt: opts.CreatedAt}, nil
}

func PutEvent(ctx context.Context, w RecordWriter, repo, rkey string, opts EventOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildEvent(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	value, err := EncodeRecord(record)
	if err != nil {
		return SignedRecord{}, err
	}
	signed, err := SignRecord(value, repo, signer)
	if err != nil {
		return SignedRecord{}, err
	}
	ref, err := w.PutRecord(ctx, repo, EventNSID, rkey, signed)
	if err != nil {
		return SignedRecord{}, err
	}
	return SignedRecord{URI: ref.URI, CID: ref.CID, Record: signed}, nil
}

func BuildOffering(endpointURL string, appliesTo []string, createdAt, refreshedAt string) (Offering, error) {
	endpointURL = strings.TrimSpace(endpointURL)
	if err := validateOptionalServiceRef(endpointURL); err != nil {
		return Offering{}, err
	}
	if err := validateAppliesTo(appliesTo); err != nil {
		return Offering{}, err
	}
	return Offering{EndpointURL: endpointURL, AppliesTo: appliesTo, CreatedAt: isoOrNow(createdAt), RefreshedAt: refreshedAt}, nil
}

func BuildHeartbeat(offeringURI, createdAt string) (Heartbeat, error) {
	if err := validateRecordURI(offeringURI, "heartbeat offering"); err != nil {
		return Heartbeat{}, err
	}
	return Heartbeat{Offering: offeringURI, CreatedAt: isoOrNow(createdAt)}, nil
}

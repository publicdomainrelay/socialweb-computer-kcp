package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

type RFP struct {
	Type       string           `json:"$type"`
	Domain     string           `json:"domain,omitempty"`
	Payload    StrongRef        `json:"payload"`
	SubmitBid  string           `json:"submitBid,omitempty"`
	Policies   []StrongRef      `json:"policies,omitempty"`
	CreatedAt  string           `json:"createdAt,omitempty"`
	Signatures []map[string]any `json:"signatures"`
	URI        string           `json:"-"`
	CID        string           `json:"-"`
}

type Bid struct {
	Type         string           `json:"$type"`
	RFP          StrongRef        `json:"rfp"`
	Config       *StrongRef       `json:"config,omitempty"`
	Payload      StrongRef        `json:"payload"`
	SubmitAccept string           `json:"submitAccept,omitempty"`
	CreatedAt    string           `json:"createdAt,omitempty"`
	Signatures   []map[string]any `json:"signatures"`
	URI          string           `json:"-"`
	CID          string           `json:"-"`
}

type Accept struct {
	Type        string           `json:"$type"`
	RFP         StrongRef        `json:"rfp"`
	Bid         StrongRef        `json:"bid"`
	Payload     *StrongRef       `json:"payload,omitempty"`
	SubmitEvent string           `json:"submitEvent,omitempty"`
	CreatedAt   string           `json:"createdAt,omitempty"`
	Signatures  []map[string]any `json:"signatures"`
	URI         string           `json:"-"`
	CID         string           `json:"-"`
}

type Receipt struct {
	Type        string           `json:"$type"`
	RFP         StrongRef        `json:"rfp"`
	Bid         StrongRef        `json:"bid"`
	Accept      StrongRef        `json:"accept"`
	Payload     *StrongRef       `json:"payload"`
	SubmitEvent string           `json:"submitEvent,omitempty"`
	CID         string           `json:"cid"`
	CreatedAt   string           `json:"createdAt,omitempty"`
	Signatures  []map[string]any `json:"signatures"`
	URI         string           `json:"-"`
	RecordCID   string           `json:"-"`
}

type Event struct {
	Type       string           `json:"$type"`
	Receipt    StrongRef        `json:"receipt"`
	Payload    StrongRef        `json:"payload"`
	CreatedAt  string           `json:"createdAt,omitempty"`
	Signatures []map[string]any `json:"signatures"`
	URI        string           `json:"-"`
	CID        string           `json:"-"`
}

type Offering struct {
	Type        string   `json:"$type"`
	EndpointURL string   `json:"endpointUrl"`
	AppliesTo   []string `json:"appliesTo"`
	CreatedAt   string   `json:"createdAt"`
	RefreshedAt string   `json:"refreshedAt,omitempty"`
	URI         string   `json:"-"`
	CID         string   `json:"-"`
}

type Heartbeat struct {
	Type      string `json:"$type"`
	Offering  string `json:"offering"`
	CreatedAt string `json:"createdAt"`
	URI       string `json:"-"`
	CID       string `json:"-"`
}

type Relays struct {
	Type      string   `json:"$type"`
	Relays    []string `json:"relays"`
	CreatedAt string   `json:"createdAt"`
	URI       string   `json:"-"`
	CID       string   `json:"-"`
}

type BadgeBlueKeys struct {
	Type      string `json:"$type"`
	KeyID     string `json:"keyId"`
	Name      string `json:"name,omitempty"`
	Challenge string `json:"challenge"`
	Service   string `json:"service,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	CreatedAt string `json:"createdAt"`
	URI       string `json:"-"`
	CID       string `json:"-"`
}

type BidsFree struct {
	Type       string           `json:"$type"`
	Reason     string           `json:"reason"`
	Signatures []map[string]any `json:"signatures"`
}

type BidsX402 struct {
	Type       string           `json:"$type"`
	Cost       any              `json:"cost"`
	Currency   string           `json:"currency"`
	Frequency  string           `json:"frequency"`
	Prepay     bool             `json:"prepay"`
	URL        string           `json:"url"`
	Signatures []map[string]any `json:"signatures"`
}

type AcceptsFree struct {
	Type       string           `json:"$type"`
	Bid        StrongRef        `json:"bid"`
	Payload    *StrongRef       `json:"payload,omitempty"`
	Signatures []map[string]any `json:"signatures"`
}

type AcceptsX402 struct {
	Type       string           `json:"$type"`
	Bid        StrongRef        `json:"bid"`
	Payload    *StrongRef       `json:"payload,omitempty"`
	Signatures []map[string]any `json:"signatures"`
}

type ReceiptsFree struct {
	Type       string           `json:"$type"`
	Accept     StrongRef        `json:"accept"`
	CID        string           `json:"cid"`
	Signatures []map[string]any `json:"signatures"`
}

type ReceiptsX402 struct {
	Type       string           `json:"$type"`
	Accept     StrongRef        `json:"accept"`
	CID        string           `json:"cid"`
	Signatures []map[string]any `json:"signatures"`
}

func decodeInto(value map[string]any, out any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	return nil
}

func signatureMaps(record map[string]any) ([]map[string]any, error) {
	raw, ok := record["signatures"]
	if !ok {
		return nil, fmt.Errorf("%w: missing signatures", ErrInvalidRecord)
	}
	switch v := raw.(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: signature entry is not an object", ErrInvalidRecord)
			}
			out = append(out, m)
		}
		return out, nil
	case []map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("%w: signatures is not an array", ErrInvalidRecord)
	}
}

func ValidateSignatureEntries(sigs []map[string]any, required bool) error {
	if len(sigs) == 0 {
		if required {
			return fmt.Errorf("%w: missing signatures", ErrInvalidRecord)
		}
		return nil
	}
	for _, sig := range sigs {
		if key, ok := sig["key"].(string); ok {
			if key == "" {
				return fmt.Errorf("%w: signature missing key", ErrInvalidRecord)
			}
			if _, ok := normalizeSignatureBytes(sig["signature"]); !ok {
				return fmt.Errorf("%w: signature missing signature bytes", ErrInvalidRecord)
			}
			cidStr, _ := sig["cid"].(string)
			if cidStr == "" || !IsAttestationCIDString(cidStr) {
				return fmt.Errorf("%w: signature missing valid cid", ErrInvalidRecord)
			}
			continue
		}
		uri, uok := sig["uri"].(string)
		cidStr, cok := sig["cid"].(string)
		if !uok || !cok {
			return fmt.Errorf("%w: signature entry is neither inline nor a strongRef", ErrInvalidRecord)
		}
		if _, err := ParseRecordRef(uri, cidStr); err != nil {
			return err
		}
	}
	return nil
}

func validateRecordType(value map[string]any, want string) error {
	got, _ := value["$type"].(string)
	if got != want {
		return fmt.Errorf("%w: expected $type %s, got %q", ErrInvalidRecord, want, got)
	}
	return nil
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

func validateSignatureField(value map[string]any) error {
	sigs, err := signatureMaps(value)
	if err != nil {
		return err
	}
	return ValidateSignatureEntries(sigs, true)
}

func ParseRFP(value map[string]any) (*RFP, error) {
	if err := validateRecordType(value, RFPNSID); err != nil {
		return nil, err
	}
	var out RFP
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if err := out.Payload.Validate(); err != nil {
		return nil, err
	}
	for _, p := range out.Policies {
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	if out.SubmitBid != "" {
		if err := ValidServiceRef(out.SubmitBid); err != nil {
			return nil, err
		}
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseBid(value map[string]any) (*Bid, error) {
	if err := validateRecordType(value, BidNSID); err != nil {
		return nil, err
	}
	var out Bid
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if err := validateRefCollection(out.RFP, "rfp", RFPNSID); err != nil {
		return nil, err
	}
	if err := out.Payload.Validate(); err != nil {
		return nil, err
	}
	if out.Config != nil {
		if err := out.Config.Validate(); err != nil {
			return nil, err
		}
	}
	if out.SubmitAccept != "" {
		if err := ValidServiceRef(out.SubmitAccept); err != nil {
			return nil, err
		}
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseAccept(value map[string]any) (*Accept, error) {
	if err := validateRecordType(value, AcceptNSID); err != nil {
		return nil, err
	}
	var out Accept
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if err := validateRefCollection(out.RFP, "rfp", RFPNSID); err != nil {
		return nil, err
	}
	if err := validateRefCollection(out.Bid, "bid", BidNSID); err != nil {
		return nil, err
	}
	if out.Payload != nil {
		if err := out.Payload.Validate(); err != nil {
			return nil, err
		}
	}
	if out.SubmitEvent != "" {
		if err := ValidServiceRef(out.SubmitEvent); err != nil {
			return nil, err
		}
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseReceipt(value map[string]any) (*Receipt, error) {
	if err := validateRecordType(value, ReceiptNSID); err != nil {
		return nil, err
	}
	var out Receipt
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if out.RFP.URI != "" || out.RFP.CID != "" {
		if err := validateRefCollection(out.RFP, "rfp", RFPNSID); err != nil {
			return nil, err
		}
	}
	if out.Bid.URI != "" || out.Bid.CID != "" {
		if err := validateRefCollection(out.Bid, "bid", BidNSID); err != nil {
			return nil, err
		}
	}
	if err := validateRefCollection(out.Accept, "accept", AcceptNSID); err != nil {
		return nil, err
	}
	if out.Payload != nil {
		if err := out.Payload.Validate(); err != nil {
			return nil, err
		}
	}
	if out.SubmitEvent != "" {
		if err := ValidServiceRef(out.SubmitEvent); err != nil {
			return nil, err
		}
	}
	if out.CID == "" || !IsAttestationCIDString(out.CID) {
		return nil, fmt.Errorf("%w: receipt cid is not an attestation cid", ErrInvalidRecord)
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseEvent(value map[string]any) (*Event, error) {
	if err := validateRecordType(value, EventNSID); err != nil {
		return nil, err
	}
	var out Event
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if err := validateRefCollection(out.Receipt, "receipt", ReceiptNSID); err != nil {
		return nil, err
	}
	if err := out.Payload.Validate(); err != nil {
		return nil, err
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseOffering(value map[string]any) (*Offering, error) {
	if err := validateRecordType(value, OfferingNSID); err != nil {
		return nil, err
	}
	var out Offering
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.EndpointURL) == "" {
		return nil, fmt.Errorf("%w: offering missing endpointUrl", ErrInvalidRecord)
	}
	if len(out.AppliesTo) == 0 {
		return nil, fmt.Errorf("%w: offering missing appliesTo", ErrInvalidRecord)
	}
	for _, nsid := range out.AppliesTo {
		if _, err := syntax.ParseNSID(nsid); err != nil {
			return nil, fmt.Errorf("%w: appliesTo: %v", ErrInvalidRecord, err)
		}
	}
	if out.CreatedAt == "" {
		return nil, fmt.Errorf("%w: offering missing createdAt", ErrInvalidRecord)
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	if err := validateOptionalDatetime(out.RefreshedAt, "refreshedAt"); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseHeartbeat(value map[string]any) (*Heartbeat, error) {
	if err := validateRecordType(value, HeartbeatNSID); err != nil {
		return nil, err
	}
	var out Heartbeat
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	aturi, err := syntax.ParseATURI(out.Offering)
	if err != nil || aturi.Collection() == "" || aturi.RecordKey() == "" {
		return nil, fmt.Errorf("%w: heartbeat offering is not a record AT URI", ErrInvalidRecord)
	}
	if out.CreatedAt == "" {
		return nil, fmt.Errorf("%w: heartbeat missing createdAt", ErrInvalidRecord)
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseRelays(value map[string]any) (*Relays, error) {
	if err := validateRecordType(value, RelaysNSID); err != nil {
		return nil, err
	}
	var out Relays
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if len(out.Relays) == 0 {
		return nil, fmt.Errorf("%w: relays is empty", ErrInvalidRecord)
	}
	for _, raw := range out.Relays {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("%w: relay url %q", ErrInvalidRecord, raw)
		}
	}
	if out.CreatedAt == "" {
		return nil, fmt.Errorf("%w: relays missing createdAt", ErrInvalidRecord)
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseBadgeBlueKeys(value map[string]any) (*BadgeBlueKeys, error) {
	if err := validateRecordType(value, BadgeBlueKeysNSID); err != nil {
		return nil, err
	}
	var out BadgeBlueKeys
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if out.KeyID == "" || out.Challenge == "" || out.CreatedAt == "" {
		return nil, fmt.Errorf("%w: badgeBlueKeys missing required fields", ErrInvalidRecord)
	}
	if _, err := syntax.ParseDID(out.Challenge); err != nil {
		return nil, fmt.Errorf("%w: challenge: %v", ErrInvalidRecord, err)
	}
	if err := validateOptionalDatetime(out.CreatedAt, "createdAt"); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseBidsFree(value map[string]any) (*BidsFree, error) {
	if err := validateRecordType(value, BidsFreeNSID); err != nil {
		return nil, err
	}
	var out BidsFree
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseBidsX402(value map[string]any) (*BidsX402, error) {
	if err := validateRecordType(value, BidsX402NSID); err != nil {
		return nil, err
	}
	if _, ok := value["cost"]; !ok {
		return nil, fmt.Errorf("%w: bids.x402 missing cost", ErrInvalidRecord)
	}
	var out BidsX402
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if out.Currency == "" || out.Frequency == "" || out.URL == "" {
		return nil, fmt.Errorf("%w: bids.x402 missing required fields", ErrInvalidRecord)
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseAccepts(value map[string]any) (*AcceptsFree, error) {
	got, _ := value["$type"].(string)
	if got != AcceptsFreeNSID && got != AcceptsX402NSID {
		return nil, fmt.Errorf("%w: unexpected $type %q", ErrInvalidRecord, got)
	}
	var out AcceptsFree
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if err := validateRefCollection(out.Bid, "bid", BidNSID); err != nil {
		return nil, err
	}
	if out.Payload != nil {
		if err := out.Payload.Validate(); err != nil {
			return nil, err
		}
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseReceipts(value map[string]any) (*ReceiptsFree, error) {
	var out ReceiptsFree
	if err := decodeInto(value, &out); err != nil {
		return nil, err
	}
	if out.Type != ReceiptsFreeNSID && out.Type != ReceiptsX402NSID {
		return nil, fmt.Errorf("%w: unexpected $type %q", ErrInvalidRecord, out.Type)
	}
	if err := validateRefCollection(out.Accept, "accept", AcceptNSID); err != nil {
		return nil, err
	}
	if out.CID == "" || !IsAttestationCIDString(out.CID) {
		return nil, fmt.Errorf("%w: receipts cid is not an attestation cid", ErrInvalidRecord)
	}
	if err := validateSignatureField(value); err != nil {
		return nil, err
	}
	return &out, nil
}

func refMap(ref RecordRef) (map[string]any, error) {
	if _, err := ParseRecordRef(ref.URI, ref.CID); err != nil {
		return nil, err
	}
	return NewStrongRef(ref.URI, ref.CID).Map(), nil
}

func isoOrNow(value string) string {
	if value == "" {
		return NowISO()
	}
	return value
}

type RFPOptions struct {
	Payload   RecordRef
	SubmitBid string
	Domain    string
	Policies  []RecordRef
	CreatedAt string
}

func BuildRFP(opts RFPOptions) (map[string]any, error) {
	payload, err := refMap(opts.Payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"$type":     RFPNSID,
		"payload":   payload,
		"createdAt": isoOrNow(opts.CreatedAt),
	}
	if opts.Domain != "" {
		out["domain"] = opts.Domain
	}
	if opts.SubmitBid != "" {
		if err := ValidServiceRef(opts.SubmitBid); err != nil {
			return nil, err
		}
		out["submitBid"] = opts.SubmitBid
	}
	if len(opts.Policies) > 0 {
		policies := make([]any, 0, len(opts.Policies))
		for _, p := range opts.Policies {
			m, err := refMap(p)
			if err != nil {
				return nil, err
			}
			policies = append(policies, m)
		}
		out["policies"] = policies
	}
	return out, nil
}

func CreateRFP(ctx context.Context, w RecordWriter, repo string, opts RFPOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildRFP(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return CreateSignedRecord(ctx, w, repo, RFPNSID, record, signer)
}

type BidOptions struct {
	RFP          RecordRef
	Config       *RecordRef
	Payload      RecordRef
	SubmitAccept string
	CreatedAt    string
}

func BuildBid(opts BidOptions) (map[string]any, error) {
	rfp, err := refMap(opts.RFP)
	if err != nil {
		return nil, err
	}
	payload, err := refMap(opts.Payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"$type":     BidNSID,
		"rfp":       rfp,
		"payload":   payload,
		"createdAt": isoOrNow(opts.CreatedAt),
	}
	if opts.Config != nil {
		cfg, err := refMap(*opts.Config)
		if err != nil {
			return nil, err
		}
		out["config"] = cfg
	}
	if opts.SubmitAccept != "" {
		if err := ValidServiceRef(opts.SubmitAccept); err != nil {
			return nil, err
		}
		out["submitAccept"] = opts.SubmitAccept
	}
	return out, nil
}

func CreateBid(ctx context.Context, w RecordWriter, repo string, opts BidOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildBid(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return CreateSignedRecord(ctx, w, repo, BidNSID, record, signer)
}

type AcceptOptions struct {
	RFP         RecordRef
	Bid         RecordRef
	Payload     *RecordRef
	SubmitEvent string
	CreatedAt   string
}

func BuildAccept(opts AcceptOptions) (map[string]any, error) {
	rfp, err := refMap(opts.RFP)
	if err != nil {
		return nil, err
	}
	bid, err := refMap(opts.Bid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"$type":     AcceptNSID,
		"rfp":       rfp,
		"bid":       bid,
		"createdAt": isoOrNow(opts.CreatedAt),
	}
	if opts.Payload != nil {
		payload, err := refMap(*opts.Payload)
		if err != nil {
			return nil, err
		}
		out["payload"] = payload
	}
	if opts.SubmitEvent != "" {
		if err := ValidServiceRef(opts.SubmitEvent); err != nil {
			return nil, err
		}
		out["submitEvent"] = opts.SubmitEvent
	}
	return out, nil
}

func CreateAccept(ctx context.Context, w RecordWriter, repo string, opts AcceptOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildAccept(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return CreateSignedRecord(ctx, w, repo, AcceptNSID, record, signer)
}

type ReceiptRefs struct {
	RFP         RecordRef
	Bid         RecordRef
	Accept      RecordRef
	Payload     *RecordRef
	SubmitEvent string
}

func BuildReceipt(refs ReceiptRefs, createdAt string) (map[string]any, error) {
	rfp, err := refMap(refs.RFP)
	if err != nil {
		return nil, err
	}
	bid, err := refMap(refs.Bid)
	if err != nil {
		return nil, err
	}
	accept, err := refMap(refs.Accept)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"$type":     ReceiptNSID,
		"rfp":       rfp,
		"bid":       bid,
		"accept":    accept,
		"payload":   nil,
		"createdAt": isoOrNow(createdAt),
	}
	if refs.Payload != nil {
		payload, err := refMap(*refs.Payload)
		if err != nil {
			return nil, err
		}
		out["payload"] = payload
	}
	if refs.SubmitEvent != "" {
		if err := ValidServiceRef(refs.SubmitEvent); err != nil {
			return nil, err
		}
		out["submitEvent"] = refs.SubmitEvent
	}
	return out, nil
}

type ReceiptSubject struct {
	AcceptRecord        map[string]any
	AcceptRepositoryDID string
}

func CreateReceiptRecord(ctx context.Context, w RecordWriter, repo string, refs ReceiptRefs, subject ReceiptSubject, signer *Signer) (StrongRef, error) {
	metadata, err := BuildReceipt(refs, NowISO())
	if err != nil {
		return StrongRef{}, err
	}
	return CreateRemoteProofRecord(ctx, w, repo, ReceiptNSID, metadata, RemoteProofSubject{
		SubjectRecord:        subject.AcceptRecord,
		SubjectRepositoryDID: subject.AcceptRepositoryDID,
	}, signer)
}

func CreateEvent(ctx context.Context, w RecordWriter, repo string, opts EventOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildEvent(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	return CreateSignedRecord(ctx, w, repo, EventNSID, record, signer)
}

func PutEvent(ctx context.Context, w RecordWriter, repo, rkey string, opts EventOptions, signer *Signer) (SignedRecord, error) {
	record, err := BuildEvent(opts)
	if err != nil {
		return SignedRecord{}, err
	}
	signed, err := SignRecord(record, repo, signer)
	if err != nil {
		return SignedRecord{}, err
	}
	ref, err := w.PutRecord(ctx, repo, EventNSID, rkey, signed)
	if err != nil {
		return SignedRecord{}, err
	}
	return SignedRecord{URI: ref.URI, CID: ref.CID, Record: signed}, nil
}

type EventOptions struct {
	Receipt   RecordRef
	Payload   RecordRef
	CreatedAt string
}

func BuildEvent(opts EventOptions) (map[string]any, error) {
	receipt, err := refMap(opts.Receipt)
	if err != nil {
		return nil, err
	}
	payload, err := refMap(opts.Payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"$type":   EventNSID,
		"receipt": receipt,
		"payload": payload,
	}
	if opts.CreatedAt != "" {
		out["createdAt"] = opts.CreatedAt
	}
	return out, nil
}

func BuildOffering(endpointURL string, appliesTo []string, createdAt, refreshedAt string) (map[string]any, error) {
	if strings.TrimSpace(endpointURL) == "" {
		return nil, fmt.Errorf("%w: offering missing endpointUrl", ErrInvalidRecord)
	}
	if len(appliesTo) == 0 {
		return nil, fmt.Errorf("%w: offering missing appliesTo", ErrInvalidRecord)
	}
	list := make([]any, 0, len(appliesTo))
	for _, nsid := range appliesTo {
		if _, err := syntax.ParseNSID(nsid); err != nil {
			return nil, fmt.Errorf("%w: appliesTo: %v", ErrInvalidRecord, err)
		}
		list = append(list, nsid)
	}
	out := map[string]any{
		"$type":       OfferingNSID,
		"endpointUrl": endpointURL,
		"appliesTo":   list,
		"createdAt":   isoOrNow(createdAt),
	}
	if refreshedAt != "" {
		out["refreshedAt"] = refreshedAt
	}
	return out, nil
}

func BuildHeartbeat(offeringURI, createdAt string) (map[string]any, error) {
	aturi, err := syntax.ParseATURI(offeringURI)
	if err != nil || aturi.Collection() == "" || aturi.RecordKey() == "" {
		return nil, fmt.Errorf("%w: heartbeat offering is not a record AT URI", ErrInvalidRecord)
	}
	return map[string]any{
		"$type":     HeartbeatNSID,
		"offering":  offeringURI,
		"createdAt": isoOrNow(createdAt),
	}, nil
}

func BuildRelays(relayURLs []string, createdAt string) (map[string]any, error) {
	if len(relayURLs) == 0 {
		return nil, fmt.Errorf("%w: relays is empty", ErrInvalidRecord)
	}
	list := make([]any, 0, len(relayURLs))
	for _, raw := range relayURLs {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("%w: relay url %q", ErrInvalidRecord, raw)
		}
		list = append(list, raw)
	}
	return map[string]any{
		"$type":     RelaysNSID,
		"relays":    list,
		"createdAt": isoOrNow(createdAt),
	}, nil
}

func BuildBidsFree(reason string) map[string]any {
	return map[string]any{"$type": BidsFreeNSID, "reason": reason}
}

type BidsX402Options struct {
	Cost      any
	Currency  string
	Frequency string
	Prepay    bool
	URL       string
}

func BuildBidsX402(opts BidsX402Options) (map[string]any, error) {
	if opts.Currency == "" || opts.Frequency == "" || opts.URL == "" {
		return nil, fmt.Errorf("%w: bids.x402 missing required fields", ErrInvalidRecord)
	}
	if opts.Cost == nil {
		return nil, fmt.Errorf("%w: bids.x402 missing cost", ErrInvalidRecord)
	}
	return map[string]any{
		"$type":     BidsX402NSID,
		"cost":      opts.Cost,
		"currency":  opts.Currency,
		"frequency": opts.Frequency,
		"prepay":    opts.Prepay,
		"url":       opts.URL,
	}, nil
}

func BuildAccepts(collection string, bid RecordRef, payload *RecordRef) (map[string]any, error) {
	if collection != AcceptsFreeNSID && collection != AcceptsX402NSID {
		return nil, fmt.Errorf("%w: unexpected accepts collection %q", ErrInvalidRecord, collection)
	}
	bidRef, err := refMap(bid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"$type": collection, "bid": bidRef}
	if payload != nil {
		payloadRef, err := refMap(*payload)
		if err != nil {
			return nil, err
		}
		out["payload"] = payloadRef
	}
	return out, nil
}

func BuildReceipts(collection string, accept RecordRef) (map[string]any, error) {
	if collection != ReceiptsFreeNSID && collection != ReceiptsX402NSID {
		return nil, fmt.Errorf("%w: unexpected receipts collection %q", ErrInvalidRecord, collection)
	}
	acceptRef, err := refMap(accept)
	if err != nil {
		return nil, err
	}
	return map[string]any{"$type": collection, "accept": acceptRef}, nil
}

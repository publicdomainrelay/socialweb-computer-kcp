package market

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

const dagCBORCodec = 0x71

type KeyResolver interface {
	ResolveKey(ctx context.Context, did string) (atcrypto.PublicKey, error)
}

type KeyResolverFunc func(ctx context.Context, did string) (atcrypto.PublicKey, error)

func (f KeyResolverFunc) ResolveKey(ctx context.Context, did string) (atcrypto.PublicKey, error) {
	return f(ctx, did)
}

var DefaultKeyResolver KeyResolver = KeyResolverFunc(func(_ context.Context, did string) (atcrypto.PublicKey, error) {
	return atcrypto.ParsePublicDIDKey(did)
})

type KeysForDID interface {
	KeysForDID(ctx context.Context, did string) ([]string, error)
}

type KeysForDIDFunc func(ctx context.Context, did string) ([]string, error)

func (f KeysForDIDFunc) KeysForDID(ctx context.Context, did string) ([]string, error) {
	return f(ctx, did)
}

type DIDDocumentResolver interface {
	ResolveDID(ctx context.Context, did syntax.DID) (*identity.DIDDocument, error)
}

type InlineAttestation struct {
	Type      string
	Key       string
	CID       string
	Signature []byte
	Fields    map[string]any
}

func (e InlineAttestation) Metadata() map[string]any {
	out := make(map[string]any, len(e.Fields)+4)
	maps.Copy(out, e.Fields)
	out["$type"] = e.Type
	out["key"] = e.Key
	out["cid"] = e.CID
	return out
}

func (e InlineAttestation) Map() map[string]any {
	out := e.Metadata()
	out["signature"] = map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(e.Signature)}
	return out
}

func (e InlineAttestation) IssuerOr(repositoryDID string) string {
	if v, ok := e.Fields["issuer"].(string); ok && v != "" {
		return v
	}
	return repositoryDID
}

type Signer struct {
	PrivateKey atcrypto.PrivateKey
	Issuer     string
}

func (s *Signer) PublicDID() (string, error) {
	if s == nil || s.PrivateKey == nil {
		return "", errors.New("market: nil signer")
	}
	pub, err := s.PrivateKey.PublicKey()
	if err != nil {
		return "", err
	}
	return pub.DIDKey(), nil
}

func NewSigner(privateKey atcrypto.PrivateKey, issuer string) *Signer {
	return &Signer{PrivateKey: privateKey, Issuer: issuer}
}

func NewSignerFromHex(privateKeyHex string) (*Signer, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(privateKeyHex), "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("market: private key hex: %w", err)
	}
	priv, err := atcrypto.ParsePrivateBytesK256(b)
	if err != nil {
		return nil, err
	}
	return &Signer{PrivateKey: priv}, nil
}

func GenerateSigner() (*Signer, error) {
	priv, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		return nil, err
	}
	return &Signer{PrivateKey: priv}, nil
}

func LoadOrGenerateSigner(privateKeyHex string) (*Signer, error) {
	if strings.TrimSpace(privateKeyHex) == "" {
		return GenerateSigner()
	}
	return NewSignerFromHex(privateKeyHex)
}

func SignBytes(payload []byte, privateKey atcrypto.PrivateKey) ([]byte, error) {
	return privateKey.HashAndSign(payload)
}

func normalizeForCBOR(value any) any {
	switch v := value.(type) {
	case float64:
		if !math.IsInf(v, 0) && !math.IsNaN(v) && v == math.Trunc(v) && v >= -9223372036854775808.0 && v < 9223372036854775808.0 {
			return int64(v)
		}
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		if v <= math.MaxInt64 {
			return int64(v)
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeForCBOR(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = normalizeForCBOR(item)
		}
		return out
	default:
		return v
	}
}

func DagCborCID(value map[string]any) (cid.Cid, error) {
	enc, err := atdata.MarshalCBOR(normalizeForCBOR(value).(map[string]any))
	if err != nil {
		return cid.Undef, fmt.Errorf("market: dag-cbor encode: %w", err)
	}
	prefix := cid.Prefix{Version: 1, Codec: dagCBORCodec, MhType: multihash.SHA2_256, MhLength: 32}
	c, err := prefix.Sum(enc)
	if err != nil {
		return cid.Undef, fmt.Errorf("market: cid: %w", err)
	}
	return c, nil
}

func CreateAttestationCID(record, metadata map[string]any, repository string, fields []string) (cid.Cid, error) {
	recType, _ := record["$type"].(string)
	if recType == "" {
		return cid.Undef, fmt.Errorf("%w: record is missing $type", ErrInvalidRecord)
	}
	metaType, _ := metadata["$type"].(string)
	if metaType == "" {
		return cid.Undef, fmt.Errorf("%w: attestation metadata is missing $type", ErrInvalidRecord)
	}
	preparedMetadata := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		if k == "cid" || k == "signature" {
			continue
		}
		preparedMetadata[k] = v
	}
	preparedMetadata["repository"] = repository

	prepared := make(map[string]any, len(record)+1)
	if fields != nil {
		prepared["$type"] = recType
		prepared["$sig"] = preparedMetadata
		for _, f := range fields {
			if v, ok := record[f]; ok && f != "signatures" {
				prepared[f] = v
			}
		}
	} else {
		for k, v := range record {
			if k == "signatures" {
				continue
			}
			prepared[k] = v
		}
		prepared["$sig"] = preparedMetadata
	}
	return DagCborCID(prepared)
}

func IsAttestationCIDString(value string) bool {
	c, err := cid.Parse(value)
	if err != nil {
		return false
	}
	if c.Version() != 1 || c.Type() != dagCBORCodec {
		return false
	}
	decoded, err := multihash.Decode(c.Hash())
	if err != nil {
		return false
	}
	return decoded.Code == multihash.SHA2_256 && len(decoded.Digest) == 32
}

func ParseInlineAttestation(raw map[string]any) (InlineAttestation, bool) {
	if raw == nil {
		return InlineAttestation{}, false
	}
	if _, ok := raw["uri"]; ok {
		return InlineAttestation{}, false
	}
	key, ok := raw["key"].(string)
	if !ok || key == "" {
		return InlineAttestation{}, false
	}
	sig, ok := normalizeSignatureBytes(raw["signature"])
	if !ok || len(sig) == 0 {
		return InlineAttestation{}, false
	}
	out := InlineAttestation{Key: key, Signature: sig, Fields: map[string]any{}}
	if v, ok := raw["$type"].(string); ok {
		out.Type = v
	}
	if v, ok := raw["cid"].(string); ok {
		out.CID = v
	}
	for k, v := range raw {
		switch k {
		case "$type", "key", "cid", "signature":
			continue
		}
		out.Fields[k] = v
	}
	return out, true
}

func InlineEntries(record map[string]any) []InlineAttestation {
	rawSigs, _ := record["signatures"].([]any)
	if rawSigs == nil {
		if typed, ok := record["signatures"].([]map[string]any); ok {
			rawSigs = make([]any, len(typed))
			for i := range typed {
				rawSigs[i] = typed[i]
			}
		}
	}
	out := make([]InlineAttestation, 0, len(rawSigs))
	for _, raw := range rawSigs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entry, ok := ParseInlineAttestation(m); ok {
			out = append(out, entry)
		}
	}
	return out
}

func normalizeSignatureBytes(value any) ([]byte, bool) {
	switch v := value.(type) {
	case []byte:
		return v, true
	case atdata.Bytes:
		return []byte(v), true
	case map[string]any:
		s, ok := v["$bytes"].(string)
		if !ok {
			return nil, false
		}
		return decodeBase64Loose(s)
	case string:
		return decodeBase64Loose(v)
	case nil:
		return nil, false
	default:
		return nil, false
	}
}

func decodeBase64Loose(s string) ([]byte, bool) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	return nil, false
}

func SignInline(record map[string]any, repository string, signer *Signer) (map[string]any, error) {
	if signer == nil || signer.PrivateKey == nil {
		return nil, errors.New("market: nil signer")
	}
	did, err := signer.PublicDID()
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{"$type": AttestedSignatureNSID, "key": did}
	if signer.Issuer != "" {
		metadata["issuer"] = signer.Issuer
	}
	c, err := CreateAttestationCID(record, metadata, repository, nil)
	if err != nil {
		return nil, err
	}
	sig, err := SignBytes(c.Bytes(), signer.PrivateKey)
	if err != nil {
		return nil, err
	}
	entry := map[string]any{
		"$type":     AttestedSignatureNSID,
		"key":       did,
		"cid":       c.String(),
		"signature": map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(sig)},
	}
	if signer.Issuer != "" {
		entry["issuer"] = signer.Issuer
	}
	return entry, nil
}

func SignRecord(record map[string]any, repository string, signer *Signer) (map[string]any, error) {
	entry, err := SignInline(record, repository, signer)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(record)+1)
	maps.Copy(out, record)
	existing := []any{}
	switch v := out["signatures"].(type) {
	case []any:
		existing = v
	case []map[string]any:
		for _, item := range v {
			existing = append(existing, item)
		}
	}
	next := make([]any, 0, len(existing)+1)
	next = append(next, existing...)
	next = append(next, entry)
	out["signatures"] = next
	return out, nil
}

type SignedRecord struct {
	URI    string
	CID    string
	Record map[string]any
}

func CreateSignedRecord(ctx context.Context, w RecordWriter, repo, collection string, record map[string]any, signer *Signer) (SignedRecord, error) {
	signed, err := SignRecord(record, repo, signer)
	if err != nil {
		return SignedRecord{}, err
	}
	ref, err := w.CreateRecord(ctx, repo, collection, signed)
	if err != nil {
		return SignedRecord{}, err
	}
	return SignedRecord{URI: ref.URI, CID: ref.CID, Record: signed}, nil
}

func CreateSignedRecordAt(ctx context.Context, w RecordWriter, repo, collection, rkey string, record map[string]any, signer *Signer) (SignedRecord, error) {
	signed, err := SignRecord(record, repo, signer)
	if err != nil {
		return SignedRecord{}, err
	}
	ref, err := w.PutRecord(ctx, repo, collection, rkey, signed)
	if err != nil {
		return SignedRecord{}, err
	}
	return SignedRecord{URI: ref.URI, CID: ref.CID, Record: signed}, nil
}

type RemoteProofSubject struct {
	SubjectRecord        map[string]any
	SubjectRepositoryDID string
}

func CreateRemoteProofRecord(ctx context.Context, w RecordWriter, repo, collection string, metadata map[string]any, subject RemoteProofSubject, signer *Signer) (StrongRef, error) {
	if _, ok := metadata["$type"].(string); !ok {
		return StrongRef{}, fmt.Errorf("%w: remote proof metadata is missing $type", ErrInvalidRecord)
	}
	bind, err := CreateAttestationCID(subject.SubjectRecord, metadata, subject.SubjectRepositoryDID, nil)
	if err != nil {
		return StrongRef{}, err
	}
	value := make(map[string]any, len(metadata)+1)
	maps.Copy(value, metadata)
	value["cid"] = bind.String()
	entry, err := SignInline(value, repo, signer)
	if err != nil {
		return StrongRef{}, err
	}
	value["signatures"] = []any{entry}
	return w.CreateRecord(ctx, repo, collection, value)
}

type ReceiptSubject struct {
	AcceptRecord        map[string]any
	AcceptRepositoryDID string
}

func CreateReceiptRecordAt(ctx context.Context, w RecordWriter, repo, rkey string, refs ReceiptRefs, subject ReceiptSubject, signer *Signer) (StrongRef, error) {
	receipt, err := BuildReceipt(refs, NowISO())
	if err != nil {
		return StrongRef{}, err
	}
	metadata, err := EncodeRecord(receipt)
	if err != nil {
		return StrongRef{}, err
	}
	bind, err := CreateAttestationCID(subject.AcceptRecord, metadata, subject.AcceptRepositoryDID, nil)
	if err != nil {
		return StrongRef{}, err
	}
	receipt.CID = bind.String()
	value, err := EncodeRecord(receipt)
	if err != nil {
		return StrongRef{}, err
	}
	entry, err := SignInline(value, repo, signer)
	if err != nil {
		return StrongRef{}, err
	}
	value["signatures"] = []any{entry}
	return w.PutRecord(ctx, repo, ReceiptNSID, rkey, value)
}

func ReceiptRKey(acceptURI, acceptCID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(acceptURI) + "\n" + strings.TrimSpace(acceptCID)))
	return "receipt-" + hex.EncodeToString(sum[:16])
}

func VerifyInlineAttestation(ctx context.Context, record map[string]any, entry InlineAttestation, repositoryDID string, resolver KeyResolver) bool {
	if resolver == nil {
		resolver = DefaultKeyResolver
	}
	if len(entry.Signature) == 0 || entry.Key == "" {
		return false
	}
	c, err := CreateAttestationCID(record, entry.Metadata(), repositoryDID, nil)
	if err != nil {
		return false
	}
	if entry.CID != "" && entry.CID != c.String() {
		return false
	}
	key, err := resolver.ResolveKey(ctx, entry.Key)
	if err != nil {
		return false
	}
	return key.HashAndVerify(c.Bytes(), entry.Signature) == nil
}

func VerifyRemoteProof(subjectRecord map[string]any, subjectRepositoryDID string, proofRecord map[string]any) bool {
	declared, ok := proofRecord["cid"].(string)
	if !ok || declared == "" {
		return false
	}
	metadata := make(map[string]any, len(proofRecord))
	for k, v := range proofRecord {
		switch k {
		case "cid", "signature", "signatures":
			continue
		}
		metadata[k] = v
	}
	c, err := CreateAttestationCID(subjectRecord, metadata, subjectRepositoryDID, nil)
	if err != nil {
		return false
	}
	return c.String() == declared
}

type VerifyRecordOptions struct {
	KeysForDID             KeysForDID
	KeyBoundByBadgeBlueKey func(ctx context.Context, did, key string) (bool, error)
	KeyResolver            KeyResolver
}

func VerifyRecordSignatures(ctx context.Context, record map[string]any, repositoryDID string, opts VerifyRecordOptions) bool {
	resolver := opts.KeyResolver
	if resolver == nil {
		resolver = DefaultKeyResolver
	}
	for _, entry := range InlineEntries(record) {
		if !VerifyInlineAttestation(ctx, record, entry, repositoryDID, resolver) {
			continue
		}
		if opts.KeysForDID != nil {
			allowed, err := opts.KeysForDID.KeysForDID(ctx, entry.IssuerOr(repositoryDID))
			bound := err == nil && slices.Contains(allowed, entry.Key)
			if !bound {
				if opts.KeyBoundByBadgeBlueKey == nil {
					continue
				}
				ok, berr := opts.KeyBoundByBadgeBlueKey(ctx, repositoryDID, entry.Key)
				if berr != nil || !ok {
					continue
				}
			}
		}
		return true
	}
	return false
}

func NewDIDKeyLister(dir DIDDocumentResolver) KeysForDID {
	var mu sync.Mutex
	cache := map[string][]string{}
	return KeysForDIDFunc(func(ctx context.Context, did string) ([]string, error) {
		mu.Lock()
		cached, ok := cache[did]
		mu.Unlock()
		if ok {
			return cached, nil
		}
		keys := []string{}
		if parsed, err := syntax.ParseDID(did); err == nil {
			if doc, err := dir.ResolveDID(ctx, parsed); err == nil && doc != nil {
				for _, vm := range doc.VerificationMethod {
					if vm.PublicKeyMultibase != "" {
						keys = append(keys, "did:key:"+vm.PublicKeyMultibase)
					}
				}
			}
		}
		mu.Lock()
		cache[did] = keys
		mu.Unlock()
		return keys, nil
	})
}

func NewDirectoryKeyLister(dir identity.Directory) KeysForDID {
	var mu sync.Mutex
	cache := map[string][]string{}
	return KeysForDIDFunc(func(ctx context.Context, did string) ([]string, error) {
		mu.Lock()
		cached, ok := cache[did]
		mu.Unlock()
		if ok {
			return cached, nil
		}
		keys := []string{}
		if parsed, err := syntax.ParseDID(did); err == nil {
			if ident, err := dir.LookupDID(ctx, parsed); err == nil && ident != nil {
				for _, vm := range ident.Keys {
					if vm.PublicKeyMultibase != "" {
						keys = append(keys, "did:key:"+vm.PublicKeyMultibase)
					}
				}
			}
		}
		mu.Lock()
		cache[did] = keys
		mu.Unlock()
		return keys, nil
	})
}

func AttestationVerificationMethod(controller, keypairDID, fragment string) map[string]any {
	fragment = cmp.Or(fragment, "attestation")
	return map[string]any{
		"id":                 controller + "#" + fragment,
		"type":               "Multikey",
		"controller":         controller,
		"publicKeyMultibase": strings.TrimPrefix(keypairDID, "did:key:"),
	}
}

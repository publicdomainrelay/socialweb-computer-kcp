package plc

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

type GenesisOptions struct {
	RotationKeys        []string
	VerificationMethods map[string]string
	AlsoKnownAs         []string
	Services            map[string]Service
	Signer              atcrypto.PrivateKey
}

func CreateGenesis(opts GenesisOptions) (string, *Operation, error) {
	if opts.Signer == nil {
		return "", nil, fmt.Errorf("plc: a genesis operation needs a signer")
	}
	if len(opts.RotationKeys) == 0 {
		return "", nil, fmt.Errorf("plc: a genesis operation needs at least one rotation key")
	}
	op := &Operation{
		Type:                TypeOperation,
		RotationKeys:        append([]string{}, opts.RotationKeys...),
		VerificationMethods: copyStringMap(opts.VerificationMethods),
		AlsoKnownAs:         append([]string{}, opts.AlsoKnownAs...),
		Services:            copyServiceMap(opts.Services),
	}
	if err := SignOperation(op, opts.Signer); err != nil {
		return "", nil, err
	}
	did, err := OperationDID(op)
	if err != nil {
		return "", nil, err
	}
	return did, op, nil
}

func CreateTombstone(prev string, signer atcrypto.PrivateKey) (*Operation, error) {
	if signer == nil {
		return nil, fmt.Errorf("plc: a tombstone operation needs a signer")
	}
	p := prev
	op := &Operation{Type: TypeTombstone, Prev: &p}
	if err := SignOperation(op, signer); err != nil {
		return nil, err
	}
	return op, nil
}

func SignOperation(op *Operation, signer atcrypto.PrivateKey) error {
	if op == nil {
		return fmt.Errorf("plc: nil operation")
	}
	op.raw = nil
	unsigned, err := atdata.MarshalCBOR(op.object())
	if err != nil {
		return fmt.Errorf("plc: encode unsigned operation: %w", err)
	}
	sig, err := signer.HashAndSign(unsigned)
	if err != nil {
		return fmt.Errorf("plc: sign operation: %w", err)
	}
	op.Sig = base64.RawURLEncoding.EncodeToString(sig)
	return nil
}

func OperationCID(op *Operation) (string, error) {
	if op == nil {
		return "", fmt.Errorf("plc: nil operation")
	}
	b, err := atdata.MarshalCBOR(op.SignedObject())
	if err != nil {
		return "", fmt.Errorf("plc: encode operation: %w", err)
	}
	sum := sha256.Sum256(b)
	mh, err := multihash.Encode(sum[:], multihash.SHA2_256)
	if err != nil {
		return "", err
	}
	return cid.NewCidV1(cid.DagCBOR, mh).String(), nil
}

func OperationDID(op *Operation) (string, error) {
	if op == nil {
		return "", fmt.Errorf("plc: nil operation")
	}
	b, err := atdata.MarshalCBOR(op.SignedObject())
	if err != nil {
		return "", fmt.Errorf("plc: encode operation: %w", err)
	}
	sum := sha256.Sum256(b)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:15])
	return "did:plc:" + strings.ToLower(enc), nil
}

func VerifyOperationSignature(op *Operation, rotationKeys []string) (bool, error) {
	if op == nil {
		return false, fmt.Errorf("plc: nil operation")
	}
	sig, err := decodeSignature(op.Sig)
	if err != nil {
		return false, err
	}
	unsigned, err := atdata.MarshalCBOR(op.UnsignedObject())
	if err != nil {
		return false, fmt.Errorf("plc: encode unsigned operation: %w", err)
	}
	var lastErr error
	for _, key := range rotationKeys {
		pub, err := didKeyPublic(key)
		if err != nil {
			lastErr = err
			continue
		}
		if err := pub.HashAndVerify(unsigned, sig); err == nil {
			return true, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("plc: no rotation keys")
	}
	return false, lastErr
}

func didKeyPublic(key string) (atcrypto.PublicKey, error) {
	if !strings.HasPrefix(key, "did:key:") {
		return nil, fmt.Errorf("plc: rotation key %q is not a did:key", key)
	}
	return atcrypto.ParsePublicDIDKey(key)
}

func decodeSignature(sig string) ([]byte, error) {
	if sig == "" {
		return nil, fmt.Errorf("plc: empty signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(sig)
	if err == nil {
		return b, nil
	}
	b, err = base64.URLEncoding.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("plc: signature is not base64url: %w", err)
	}
	return b, nil
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyServiceMap(in map[string]Service) map[string]Service {
	out := make(map[string]Service, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

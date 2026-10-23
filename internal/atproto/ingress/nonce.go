package ingress

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/didkey"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

var errNoDirectory = errors.New("ingress: no identity directory to resolve a signing key")

type verifyResult struct {
	OK     bool
	Key    string
	Reason string
}

type NonceStore struct {
	dir     identity.Directory
	entries *ttlcache.Cache[string, string]
}

func NewNonceStore(ttl time.Duration, dir identity.Directory) *NonceStore {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &NonceStore{
		dir:     dir,
		entries: ttlcache.New[string, string](ttlcache.Options{TTL: ttl}),
	}
}

func (n *NonceStore) Issue(key string) string {
	b := make([]byte, 64)
	_, _ = rand.Read(b)
	nonce := b64encode(b)
	n.entries.Set(nonce, key)
	return nonce
}

func (n *NonceStore) Verify(ctx context.Context, reg Registration) verifyResult {
	if reg.Key == "" {
		return verifyResult{Reason: "missing key"}
	}
	if reg.Nonce == "" {
		return verifyResult{Reason: "missing nonce"}
	}
	key, ok := n.entries.Get(reg.Nonce)
	if !ok {
		return verifyResult{Reason: "unknown or expired nonce"}
	}
	if key != reg.Key {
		return verifyResult{Reason: "nonce was issued for a different key"}
	}
	if len(reg.Signatures) == 0 {
		return verifyResult{Reason: "no signatures provided"}
	}
	nonceBytes, err := b64decode(reg.Nonce)
	if err != nil {
		return verifyResult{Reason: "nonce is not base64"}
	}
	if n.verifySignatures(ctx, nonceBytes, reg.Signatures) {
		n.consume(reg.Nonce)
		return verifyResult{OK: true, Key: reg.Key}
	}
	if n.purgeSignatureKeys(ctx, reg.Signatures) && n.verifySignatures(ctx, nonceBytes, reg.Signatures) {
		n.consume(reg.Nonce)
		return verifyResult{OK: true, Key: reg.Key}
	}
	return verifyResult{Reason: "no signature verifies over the nonce"}
}

func (n *NonceStore) consume(nonce string) {
	n.entries.Delete(nonce)
}

func (n *NonceStore) verifySignatures(ctx context.Context, nonceBytes []byte, signatures []Signature) bool {
	for _, sig := range signatures {
		if sig.Key == "" || sig.Signature == "" {
			continue
		}
		pub, err := resolveRegistrationKey(ctx, sig.Key, n.dir)
		if err != nil {
			continue
		}
		sigBytes, err := b64decode(sig.Signature)
		if err != nil {
			continue
		}
		if err := pub.HashAndVerify(nonceBytes, sigBytes); err == nil {
			return true
		}
	}
	return false
}

func (n *NonceStore) purgeSignatureKeys(ctx context.Context, signatures []Signature) bool {
	if n.dir == nil {
		return false
	}
	purged := false
	for _, sig := range signatures {
		if sig.Key == "" || strings.HasPrefix(sig.Key, didkey.Prefix) {
			continue
		}
		did, err := syntax.ParseDID(sig.Key)
		if err != nil {
			continue
		}
		if err := n.dir.Purge(ctx, did.AtIdentifier()); err == nil {
			purged = true
		}
	}
	return purged
}

func resolveRegistrationKey(ctx context.Context, key string, dir identity.Directory) (atcrypto.PublicKey, error) {
	if strings.HasPrefix(key, didkey.Prefix) {
		return didkey.ParsePublic(key)
	}
	if dir == nil {
		return nil, errNoDirectory
	}
	return resolveSigningKey(ctx, key, dir)
}

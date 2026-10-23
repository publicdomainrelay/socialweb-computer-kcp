package ingress

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/didkey"
)

var errNoDirectory = errors.New("ingress: no identity directory to resolve a signing key")

type verifyResult struct {
	OK     bool
	Key    string
	Reason string
}

type nonceEntry struct {
	key       string
	expiresAt time.Time
}

type NonceStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	dir     identity.Directory
	entries map[string]nonceEntry
	stop    chan struct{}
	once    sync.Once
}

func NewNonceStore(ttl time.Duration, dir identity.Directory) *NonceStore {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	n := &NonceStore{
		ttl:     ttl,
		dir:     dir,
		entries: map[string]nonceEntry{},
		stop:    make(chan struct{}),
	}
	go n.purge()
	return n
}

func (n *NonceStore) Close() {
	n.once.Do(func() { close(n.stop) })
}

func (n *NonceStore) purge() {
	interval := min(n.ttl, 30*time.Second)
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case now := <-t.C:
			n.mu.Lock()
			for nonce, entry := range n.entries {
				if entry.expiresAt.Before(now) {
					delete(n.entries, nonce)
				}
			}
			n.mu.Unlock()
		}
	}
}

func (n *NonceStore) Issue(key string) string {
	b := make([]byte, 64)
	_, _ = rand.Read(b)
	nonce := b64encode(b)
	n.mu.Lock()
	n.entries[nonce] = nonceEntry{key: key, expiresAt: time.Now().Add(n.ttl)}
	n.mu.Unlock()
	return nonce
}

func (n *NonceStore) Verify(ctx context.Context, reg Registration) verifyResult {
	if reg.Key == "" {
		return verifyResult{Reason: "missing key"}
	}
	if reg.Nonce == "" {
		return verifyResult{Reason: "missing nonce"}
	}
	n.mu.Lock()
	entry, ok := n.entries[reg.Nonce]
	n.mu.Unlock()
	if !ok {
		return verifyResult{Reason: "unknown or expired nonce"}
	}
	if entry.key != reg.Key {
		return verifyResult{Reason: "nonce was issued for a different key"}
	}
	if entry.expiresAt.Before(time.Now()) {
		n.mu.Lock()
		delete(n.entries, reg.Nonce)
		n.mu.Unlock()
		return verifyResult{Reason: "nonce expired"}
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
	n.mu.Lock()
	delete(n.entries, nonce)
	n.mu.Unlock()
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

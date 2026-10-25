package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	sessionPrefix  = "session/"
	statusActive   = "active"
	statusExpired  = "expired"
	refreshLeeway  = 60 * time.Second
	refreshTimeout = 30 * time.Second
)

type sessionEntry struct {
	DID                string          `json:"did"`
	Handle             string          `json:"handle"`
	PDS                string          `json:"pds"`
	Issuer             string          `json:"issuer"`
	TokenEndpoint      string          `json:"token_endpoint"`
	RevocationEndpoint string          `json:"revocation_endpoint"`
	Scope              string          `json:"scope"`
	AccessToken        string          `json:"access_token"`
	RefreshToken       string          `json:"refresh_token"`
	ExpiresAt          time.Time       `json:"expires_at"`
	DPoPJWK            json.RawMessage `json:"dpop_jwk"`
	Status             string          `json:"status"`
	StatusReason       string          `json:"status_reason,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func (s *sessionEntry) dpopKey() (*jose.JSONWebKey, error) {
	var jwk jose.JSONWebKey
	if err := json.Unmarshal(s.DPoPJWK, &jwk); err != nil {
		return nil, err
	}
	return &jwk, nil
}

func getSession(ctx context.Context, st logical.Storage, did string) (*sessionEntry, error) {
	raw, err := st.Get(ctx, sessionPrefix+did)
	if err != nil || raw == nil {
		return nil, err
	}
	var s sessionEntry
	if err := raw.DecodeJSON(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func putSession(ctx context.Context, st logical.Storage, s *sessionEntry) error {
	s.UpdatedAt = time.Now().UTC()
	entry, err := logical.StorageEntryJSON(sessionPrefix+s.DID, s)
	if err != nil {
		return err
	}
	return st.Put(ctx, entry)
}

type sessionLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newSessionLocks() *sessionLocks { return &sessionLocks{locks: map[string]*sync.Mutex{}} }

func (l *sessionLocks) forDID(did string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.locks[did]
	if !ok {
		m = &sync.Mutex{}
		l.locks[did] = m
	}
	return m
}

type nonceCache struct {
	mu     sync.Mutex
	nonces map[string]string
}

func newNonceCache() *nonceCache { return &nonceCache{nonces: map[string]string{}} }

type didNonces struct {
	cache *nonceCache
	did   string
}

func (n *nonceCache) forDID(did string) nonceStore { return didNonces{cache: n, did: did} }

func (d didNonces) get(origin string) string {
	d.cache.mu.Lock()
	defer d.cache.mu.Unlock()
	return d.cache.nonces[d.did+"|"+origin]
}

func (d didNonces) set(origin, nonce string) {
	d.cache.mu.Lock()
	defer d.cache.mu.Unlock()
	d.cache.nonces[d.did+"|"+origin] = nonce
}

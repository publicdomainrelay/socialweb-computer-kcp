package market

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/mr-tron/base58"
)

func Base58BTCEncode(b []byte) string {
	return base58.Encode(b)
}

func BadgeBlueKeysRkey(did, keyID string) string {
	sum := sha256.Sum256([]byte(did + ":" + keyID))
	rkey := base58.Encode(sum[:24])
	if len(rkey) > 32 {
		rkey = rkey[:32]
	}
	return rkey
}

func KeyBoundByBadgeBlueKey(ctx context.Context, getter RecordGetter, did, key string) bool {
	if getter == nil {
		return false
	}
	rkey := BadgeBlueKeysRkey(did, key)
	rec, err := getter.GetRecord(ctx, did, BadgeBlueKeysNSID, rkey)
	if err != nil {
		return false
	}
	keyID, _ := rec.Value["keyId"].(string)
	challenge, _ := rec.Value["challenge"].(string)
	return keyID == key && challenge == did
}

type BadgeBlueKeyBinding struct {
	getter RecordGetter
	mu     sync.Mutex
	cache  map[string]bool
}

func NewBadgeBlueKeyBinding(getter RecordGetter) *BadgeBlueKeyBinding {
	return &BadgeBlueKeyBinding{getter: getter, cache: map[string]bool{}}
}

func (b *BadgeBlueKeyBinding) Check(ctx context.Context, did, key string) (bool, error) {
	cacheKey := did + "#" + key
	b.mu.Lock()
	cached, ok := b.cache[cacheKey]
	b.mu.Unlock()
	if ok {
		return cached, nil
	}
	bound := KeyBoundByBadgeBlueKey(ctx, b.getter, did, key)
	b.mu.Lock()
	b.cache[cacheKey] = bound
	b.mu.Unlock()
	return bound, nil
}

type BadgeBlueKeysRecordOptions struct {
	DID       string
	KeyID     string
	Service   string
	CreatedAt string
}

func BuildBadgeBlueKeysRecord(opts BadgeBlueKeysRecordOptions) map[string]any {
	createdAt := opts.CreatedAt
	if createdAt == "" {
		createdAt = NowISO()
	}
	return map[string]any{
		"$type":     BadgeBlueKeysNSID,
		"keyId":     opts.KeyID,
		"challenge": opts.DID,
		"service":   opts.Service,
		"createdAt": createdAt,
	}
}

func CreateBadgeBlueKeysRecord(ctx context.Context, w RecordWriter, opts BadgeBlueKeysRecordOptions) (StrongRef, error) {
	if w == nil {
		return StrongRef{}, fmt.Errorf("%w: nil writer", ErrInvalidRecord)
	}
	if opts.DID == "" || opts.KeyID == "" {
		return StrongRef{}, fmt.Errorf("%w: badgeBlueKeys needs did and keyId", ErrInvalidRecord)
	}
	rkey := BadgeBlueKeysRkey(opts.DID, opts.KeyID)
	return w.PutRecord(ctx, opts.DID, BadgeBlueKeysNSID, rkey, BuildBadgeBlueKeysRecord(opts))
}

package guestidentity

import (
	"container/list"
	"sync"
	"time"
)

const DefaultMaxBucketKeys = 4096

type TokenBuckets struct {
	capacity float64

	refill float64

	maxKeys int

	mu    sync.Mutex
	held  map[string]*tokenBucket
	order *list.List
}

type tokenBucket struct {
	key    string
	tokens float64
	at     time.Time
	entry  *list.Element
}

func NewTokenBuckets(capacity, refillPerSecond float64, maxKeys int) *TokenBuckets {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxBucketKeys
	}
	return &TokenBuckets{capacity: capacity, refill: refillPerSecond, maxKeys: maxKeys,
		held: map[string]*tokenBucket{}, order: list.New()}
}

func LoginRateLimits() *TokenBuckets { return NewTokenBuckets(30, 1, DefaultMaxBucketKeys) }

func OIDCTokenRateLimits() *TokenBuckets { return NewTokenBuckets(20, 0.5, DefaultMaxBucketKeys) }

func PublicRateLimits() *TokenBuckets { return NewTokenBuckets(60, 10, DefaultMaxBucketKeys) }

func (b *TokenBuckets) Take(key string, now time.Time) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	held, ok := b.held[key]
	if !ok {
		held = &tokenBucket{key: key, tokens: b.capacity, at: now}
		held.entry = b.order.PushBack(held)
		b.held[key] = held
	} else {
		earned := now.Sub(held.at).Seconds() * b.refill
		if earned > 0 {
			held.tokens += earned
		}
		if held.tokens > b.capacity {
			held.tokens = b.capacity
		}
		held.at = now
		b.order.MoveToBack(held.entry)
	}
	if !ok {
		for len(b.held) > b.maxKeys {
			oldest := b.order.Front()
			if oldest == nil {
				break
			}
			bucket := oldest.Value.(*tokenBucket)
			b.order.Remove(oldest)
			delete(b.held, bucket.key)
		}
	}
	if held.tokens < 1 {
		return false
	}
	held.tokens--
	return true
}

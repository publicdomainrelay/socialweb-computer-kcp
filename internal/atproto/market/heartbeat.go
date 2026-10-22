package market

import (
	"context"
	"sync"
	"time"
)

const HeartbeatEventDebounce = 15 * time.Second

type heartbeatEntry struct {
	at        time.Time
	checkedAt time.Time
	valid     bool
}

type HeartbeatCache struct {
	Getter RecordGetter
	mu     sync.Mutex
	held   map[string]heartbeatEntry
}

func NewHeartbeatCache(getter RecordGetter) *HeartbeatCache {
	return &HeartbeatCache{Getter: getter, held: map[string]heartbeatEntry{}}
}

func heartbeatFreshAt(entry heartbeatEntry, now time.Time) bool {
	return entry.valid && now.Sub(entry.at) <= HeartbeatTTL
}

func (c *HeartbeatCache) read(ctx context.Context, did string, now time.Time) heartbeatEntry {
	entry := heartbeatEntry{checkedAt: now}
	if c.Getter == nil {
		return entry
	}
	rec, err := c.Getter.GetRecord(ctx, did, HeartbeatNSID, HeartbeatRKey)
	if err != nil {
		return entry
	}
	value, _ := rec.Value["createdAt"].(string)
	at, err := ParseTime(value)
	if err != nil || at.After(now.Add(HeartbeatMaxSkew)) {
		return entry
	}
	entry.at = at
	entry.valid = true
	return entry
}

func (c *HeartbeatCache) FreshDIDs(ctx context.Context, dids []string, now time.Time) []string {
	out := make([]string, 0, len(dids))
	for _, did := range dids {
		c.mu.Lock()
		entry, ok := c.held[did]
		c.mu.Unlock()
		if !ok || !(heartbeatFreshAt(entry, now) || now.Sub(entry.checkedAt) < HeartbeatInterval) {
			entry = c.read(ctx, did, now)
			c.mu.Lock()
			c.held[did] = entry
			c.mu.Unlock()
		}
		if heartbeatFreshAt(entry, now) {
			out = append(out, did)
		}
	}
	return out
}

func (c *HeartbeatCache) HandleEvent(ctx context.Context, event WatchEvent) {
	if event.Collection != HeartbeatNSID || event.DID == "" {
		return
	}
	now := time.Now()
	if event.Operation == "delete" {
		c.mu.Lock()
		c.held[event.DID] = heartbeatEntry{checkedAt: now}
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	held, ok := c.held[event.DID]
	c.mu.Unlock()
	if ok && now.Sub(held.checkedAt) < HeartbeatEventDebounce {
		return
	}
	entry := c.read(ctx, event.DID, now)
	c.mu.Lock()
	c.held[event.DID] = entry
	c.mu.Unlock()
}

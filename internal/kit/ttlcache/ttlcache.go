package ttlcache

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type Options struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

type Cache[K comparable, V any] struct {
	ttl       time.Duration
	max       int
	now       func() time.Time
	mu        sync.Mutex
	order     *list.List
	items     map[K]*list.Element
	flights   map[K]*flight
	seq       uint64
	nextPrune int
	onEvict   func(K, V)
	group     singleflight.Group
}

type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
}

type flight struct {
	id    string
	stale bool
}

const minPrune = 64

func New[K comparable, V any](opts Options) *Cache[K, V] {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache[K, V]{
		ttl:       opts.TTL,
		max:       opts.MaxEntries,
		now:       opts.Now,
		order:     list.New(),
		items:     make(map[K]*list.Element),
		flights:   make(map[K]*flight),
		nextPrune: minPrune,
	}
}

func (c *Cache[K, V]) OnEvict(f func(K, V)) *Cache[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEvict = f
	return c
}

func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

func (c *Cache[K, V]) Set(key K, value V) {
	c.SetTTL(key, value, 0)
}

func (c *Cache[K, V]) SetTTL(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value, ttl)
}

func (c *Cache[K, V]) Add(key K, value V, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.getLocked(key); ok {
		return false
	}
	c.setLocked(key, value, ttl)
	return true
}

func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
	if f, ok := c.flights[key]; ok {
		f.stale = true
		delete(c.flights, key)
	}
}

func (c *Cache[K, V]) DeleteFunc(drop func(K, V) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*entry[K, V])
		if drop(e.key, e.value) {
			c.removeLocked(el)
			dropped++
		}
		el = next
	}
	return dropped
}

func (c *Cache[K, V]) DeleteKeys(drop func(K) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for key, f := range c.flights {
		if drop(key) {
			f.stale = true
			delete(c.flights, key)
			dropped++
		}
	}
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if drop(el.Value.(*entry[K, V]).key) {
			c.removeLocked(el)
			dropped++
		}
		el = next
	}
	return dropped
}

func (c *Cache[K, V]) Range(visit func(K, V) bool) {
	c.mu.Lock()
	now := c.now()
	keys := make([]K, 0, len(c.items))
	values := make([]V, 0, len(c.items))
	for el := c.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry[K, V])
		if e.expired(now) {
			continue
		}
		keys = append(keys, e.key)
		values = append(values, e.value)
	}
	c.mu.Unlock()
	for i := range keys {
		if !visit(keys[i], values[i]) {
			return
		}
	}
}

func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	return len(c.items)
}

func (c *Cache[K, V]) Prune() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pruneLocked()
}

func (c *Cache[K, V]) Loading() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.flights)
}

func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K, load func(context.Context) (V, time.Duration, error)) (V, error) {
	c.mu.Lock()
	if value, ok := c.getLocked(key); ok {
		c.mu.Unlock()
		return value, nil
	}
	f, ok := c.flights[key]
	if !ok {
		c.seq++
		f = &flight{id: strconv.FormatUint(c.seq, 36)}
		c.flights[key] = f
	}
	c.mu.Unlock()
	ch := c.group.DoChan(f.id, func() (any, error) {
		value, ttl, err := load(ctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.flights[key] == f {
			delete(c.flights, key)
		}
		if err == nil && !f.stale {
			c.setLocked(key, value, ttl)
		}
		return value, err
	})
	select {
	case res := <-ch:
		value, _ := res.Val.(V)
		return value, res.Err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

func (c *Cache[K, V]) getLocked(key K) (V, bool) {
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if e.expired(c.now()) {
		c.removeLocked(el)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

func (c *Cache[K, V]) setLocked(key K, value V, ttl time.Duration) {
	if ttl == 0 {
		ttl = c.ttl
	}
	if ttl < 0 {
		if el, ok := c.items[key]; ok {
			c.removeLocked(el)
		}
		return
	}
	var expires time.Time
	if ttl > 0 {
		expires = c.now().Add(ttl)
	}
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		if c.onEvict != nil {
			c.onEvict(e.key, e.value)
		}
		e.value = value
		e.expires = expires
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, expires: expires})
	if len(c.items) >= c.nextPrune {
		c.pruneLocked()
		c.nextPrune = max(2*len(c.items), minPrune)
	}
	for c.max > 0 && len(c.items) > c.max {
		c.removeLocked(c.order.Back())
	}
}

func (c *Cache[K, V]) pruneLocked() int {
	now := c.now()
	dropped := 0
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(*entry[K, V]).expired(now) {
			c.removeLocked(el)
			dropped++
		}
		el = next
	}
	return dropped
}

func (c *Cache[K, V]) removeLocked(el *list.Element) {
	e := el.Value.(*entry[K, V])
	delete(c.items, e.key)
	c.order.Remove(el)
	if c.onEvict != nil {
		c.onEvict(e.key, e.value)
	}
}

func (e *entry[K, V]) expired(now time.Time) bool {
	return !e.expires.IsZero() && !now.Before(e.expires)
}

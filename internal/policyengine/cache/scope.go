package cache

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
)

type Identity struct {
	Kind string
	URI  string
	CID  string
	Name string
}

func RefIdentity(ref atproto.StrongRef) Identity {
	return Identity{Kind: "ref", URI: ref.URI, CID: ref.CID}
}

func NameIdentity(name string) Identity {
	return Identity{Kind: "name", Name: name}
}

type ScopeOptions[T any] struct {
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	MaxEntries  int
	TTLFor      func(T) time.Duration
	Log         func(level, msg string, meta map[string]any)
}

type scopeEntry[T any] struct {
	value    T
	cachedAt time.Time
	ttl      time.Duration
	did      string
}

type scopeFlight[T any] struct {
	did   string
	stale bool
	done  chan struct{}
	value T
	err   error
}

type Scope[T any] struct {
	mu          sync.Mutex
	entries     map[string]scopeEntry[T]
	order       []string
	byDID       map[string]map[string]struct{}
	flights     map[string]*scopeFlight[T]
	positiveTTL time.Duration
	negativeTTL time.Duration
	maxEntries  int
	ttlFor      func(T) time.Duration
	log         func(level, msg string, meta map[string]any)
}

func NewScope[T any](opts ScopeOptions[T]) *Scope[T] {
	if opts.PositiveTTL <= 0 {
		opts.PositiveTTL = 5 * time.Minute
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = 30 * time.Second
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 10000
	}
	if opts.TTLFor == nil {
		opts.TTLFor = func(T) time.Duration { return opts.PositiveTTL }
	}
	if opts.Log == nil {
		opts.Log = func(string, string, map[string]any) {}
	}
	return &Scope[T]{
		entries:     make(map[string]scopeEntry[T]),
		byDID:       make(map[string]map[string]struct{}),
		flights:     make(map[string]*scopeFlight[T]),
		positiveTTL: opts.PositiveTTL,
		negativeTTL: opts.NegativeTTL,
		maxEntries:  opts.MaxEntries,
		ttlFor:      opts.TTLFor,
		log:         opts.Log,
	}
}

func (s *Scope[T]) Key(id Identity, did string, args map[string]any) string {
	base := ""
	if id.Kind == "ref" {
		base = "r:" + id.URI + "/" + id.CID
	} else {
		base = "n:" + id.Name
	}
	suffix := ""
	if len(args) > 0 {
		if data, err := marshalJSON(args); err == nil {
			suffix = ":" + HashString(data)
		}
	}
	return base + "|" + did + suffix
}

func (s *Scope[T]) Get(id Identity, did string, args map[string]any) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(s.Key(id, did, args))
}

func (s *Scope[T]) getLocked(key string) (T, bool) {
	entry, ok := s.entries[key]
	if !ok {
		var zero T
		return zero, false
	}
	if entry.ttl > 0 && time.Since(entry.cachedAt) > entry.ttl {
		s.deleteLocked(key)
		var zero T
		return zero, false
	}
	s.touchLocked(key)
	return entry.value, true
}

func (s *Scope[T]) Set(id Identity, did string, args map[string]any, value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(s.Key(id, did, args), did, value)
}

func (s *Scope[T]) setLocked(key, did string, value T) {
	ttl := s.ttlFor(value)
	if ttl <= 0 {
		ttl = s.positiveTTL
	}
	if _, exists := s.entries[key]; exists {
		s.deleteLocked(key)
	}
	s.entries[key] = scopeEntry[T]{value: value, cachedAt: time.Now(), ttl: ttl, did: did}
	s.order = append(s.order, key)
	index := s.byDID[did]
	if index == nil {
		index = make(map[string]struct{})
		s.byDID[did] = index
	}
	index[key] = struct{}{}
	for len(s.entries) > s.maxEntries {
		s.deleteLocked(s.order[0])
	}
}

func (s *Scope[T]) deleteLocked(key string) {
	entry, ok := s.entries[key]
	if !ok {
		return
	}
	delete(s.entries, key)
	for i, existing := range s.order {
		if existing == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if index := s.byDID[entry.did]; index != nil {
		delete(index, key)
		if len(index) == 0 {
			delete(s.byDID, entry.did)
		}
	}
}

func (s *Scope[T]) touchLocked(key string) {
	for i, existing := range s.order {
		if existing == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			s.order = append(s.order, key)
			return
		}
	}
}

func (s *Scope[T]) Once(ctx context.Context, id Identity, did string, args map[string]any, work func(context.Context) (T, error)) (T, error) {
	key := s.Key(id, did, args)
	s.mu.Lock()
	if entry, ok := s.getLocked(key); ok {
		s.mu.Unlock()
		return entry, nil
	}
	if flight, ok := s.flights[key]; ok {
		s.mu.Unlock()
		select {
		case <-flight.done:
			return flight.value, flight.err
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
	flight := &scopeFlight[T]{did: did, done: make(chan struct{})}
	s.flights[key] = flight
	s.mu.Unlock()

	value, err := work(ctx)

	s.mu.Lock()
	flight.value = value
	flight.err = err
	if s.flights[key] == flight {
		delete(s.flights, key)
	}
	if err == nil && !flight.stale {
		s.setLocked(key, did, value)
	}
	close(flight.done)
	s.mu.Unlock()
	return value, err
}

func (s *Scope[T]) ApplyEvent(did, rkey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range []string{did, rkey} {
		if other == "" {
			continue
		}
		dropped := 0
		for key, flight := range s.flights {
			if flight.did != other {
				continue
			}
			flight.stale = true
			delete(s.flights, key)
			dropped++
		}
		if keys := s.byDID[other]; len(keys) > 0 {
			snapshot := make([]string, 0, len(keys))
			for key := range keys {
				snapshot = append(snapshot, key)
			}
			for _, key := range snapshot {
				s.deleteLocked(key)
				dropped++
			}
		}
		if dropped > 0 {
			s.log("debug", "scope cache invalidated", map[string]any{"did": other, "dropped": dropped})
		}
	}
}

func (s *Scope[T]) Stats() (entries, inflight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), len(s.flights)
}

func marshalJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

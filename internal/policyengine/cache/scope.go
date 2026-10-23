package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
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

type scopeKey struct {
	key string
	did string
}

type Scope[T any] struct {
	entries     *ttlcache.Cache[scopeKey, T]
	positiveTTL time.Duration
	ttlFor      func(T) time.Duration
	log         func(level, msg string, meta map[string]any)
}

func NewScope[T any](opts ScopeOptions[T]) *Scope[T] {
	opts.PositiveTTL = opt.Positive(opts.PositiveTTL, 5*time.Minute)
	opts.MaxEntries = opt.Positive(opts.MaxEntries, 10000)
	if opts.TTLFor == nil {
		opts.TTLFor = func(T) time.Duration { return opts.PositiveTTL }
	}
	if opts.Log == nil {
		opts.Log = func(string, string, map[string]any) {}
	}
	return &Scope[T]{
		entries:     ttlcache.New[scopeKey, T](ttlcache.Options{TTL: opts.PositiveTTL, MaxEntries: opts.MaxEntries}),
		positiveTTL: opts.PositiveTTL,
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

func (s *Scope[T]) scopeKey(id Identity, did string, args map[string]any) scopeKey {
	return scopeKey{key: s.Key(id, did, args), did: did}
}

func (s *Scope[T]) ttl(value T) time.Duration {
	if ttl := s.ttlFor(value); ttl > 0 {
		return ttl
	}
	return s.positiveTTL
}

func (s *Scope[T]) Get(id Identity, did string, args map[string]any) (T, bool) {
	return s.entries.Get(s.scopeKey(id, did, args))
}

func (s *Scope[T]) Set(id Identity, did string, args map[string]any, value T) {
	s.entries.SetTTL(s.scopeKey(id, did, args), value, s.ttl(value))
}

func (s *Scope[T]) Once(ctx context.Context, id Identity, did string, args map[string]any, work func(context.Context) (T, error)) (T, error) {
	return s.entries.GetOrLoad(ctx, s.scopeKey(id, did, args), func(ctx context.Context) (T, time.Duration, error) {
		value, err := work(ctx)
		return value, s.ttl(value), err
	})
}

func (s *Scope[T]) ApplyEvent(did, rkey string) {
	for _, other := range []string{did, rkey} {
		if other == "" {
			continue
		}
		dropped := s.entries.DeleteKeys(func(key scopeKey) bool { return key.did == other })
		if dropped > 0 {
			s.log("debug", "scope cache invalidated", map[string]any{"did": other, "dropped": dropped})
		}
	}
}

func (s *Scope[T]) Stats() (entries, inflight int) {
	return s.entries.Len(), s.entries.Loading()
}

func marshalJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

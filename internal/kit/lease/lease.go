package lease

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type Fetch[T any] func(context.Context) (T, time.Time, error)

// Source holds one leased value. A zero expiry from fetch never expires.
// Get renews at 2/3 of the lifetime; inside that window one caller renews
// while the others keep the current value, and a failed renewal keeps it.
type Source[T any] struct {
	fetch    Fetch[T]
	now      func() time.Time
	mu       sync.Mutex
	value    T
	valid    bool
	renewAt  time.Time
	expires  time.Time
	renewing bool
	group    singleflight.Group
}

const flightKey = "lease"

func New[T any](fetch Fetch[T]) *Source[T] {
	return &Source[T]{fetch: fetch, now: time.Now}
}

func NewWithClock[T any](fetch Fetch[T], now func() time.Time) *Source[T] {
	if now == nil {
		now = time.Now
	}
	return &Source[T]{fetch: fetch, now: now}
}

func (s *Source[T]) Get(ctx context.Context) (T, error) {
	s.mu.Lock()
	now := s.now()
	if s.valid && (s.expires.IsZero() || now.Before(s.expires)) {
		value := s.value
		if s.renewAt.IsZero() || now.Before(s.renewAt) || s.renewing {
			s.mu.Unlock()
			return value, nil
		}
		s.renewing = true
		s.mu.Unlock()
		fresh, err := s.load(ctx)
		if err != nil {
			return value, nil
		}
		return fresh, nil
	}
	s.mu.Unlock()
	return s.load(ctx)
}

func (s *Source[T]) Peek() (T, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.valid || (!s.expires.IsZero() && !s.now().Before(s.expires)) {
		var zero T
		return zero, time.Time{}, false
	}
	return s.value, s.expires, true
}

func (s *Source[T]) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	s.value = zero
	s.valid = false
	s.renewAt = time.Time{}
	s.expires = time.Time{}
}

func (s *Source[T]) load(ctx context.Context) (T, error) {
	ch := s.group.DoChan(flightKey, func() (any, error) {
		start := s.now()
		value, expires, err := s.fetch(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.renewing = false
		if err != nil {
			return value, err
		}
		s.value = value
		s.valid = true
		s.expires = expires
		s.renewAt = time.Time{}
		if !expires.IsZero() {
			s.renewAt = start.Add(expires.Sub(start) * 2 / 3)
		}
		return value, nil
	})
	select {
	case res := <-ch:
		value, _ := res.Val.(T)
		return value, res.Err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

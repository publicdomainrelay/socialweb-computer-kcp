package eventstream

import (
	"sync"
	"time"
)

type seenEntry struct {
	at time.Time

	source string

	drops int

	settled bool

	ok bool

	done chan struct{}
}

type seenOrder struct {
	key string

	at time.Time
}

type duplicate struct {
	First string

	Drops int

	Dropped int64
}

type seenSet struct {
	window time.Duration

	limit int

	mu sync.Mutex

	entries map[string]*seenEntry

	order []seenOrder

	dropped int64
}

func newSeenSet(window time.Duration, limit int) *seenSet {
	return &seenSet{window: window, limit: limit, entries: map[string]*seenEntry{}}
}

func (s *seenSet) evictFront() {
	front := s.order[0]
	s.order = s.order[1:]
	if e, ok := s.entries[front.key]; ok && e.at.Equal(front.at) {
		delete(s.entries, front.key)
	}
}

func (s *seenSet) claim(key, source string, now time.Time) (*seenEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.order) > 0 && now.Sub(s.order[0].at) >= s.window {
		s.evictFront()
	}
	if e, found := s.entries[key]; found && (!e.settled || now.Sub(e.at) < s.window) {
		return e, false
	}
	e := &seenEntry{at: now, source: source, done: make(chan struct{})}
	s.entries[key] = e
	s.order = append(s.order, seenOrder{key: key, at: now})
	for len(s.order) > s.limit {
		s.evictFront()
	}
	return e, true
}

func (s *seenSet) finish(key string, e *seenEntry, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.settled, e.ok = true, ok
	if !ok && s.entries[key] == e {
		delete(s.entries, key)
	}
	close(e.done)
}

func (s *seenSet) drop(e *seenEntry) duplicate {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.drops++
	s.dropped++
	return duplicate{First: e.source, Drops: e.drops, Dropped: s.dropped}
}

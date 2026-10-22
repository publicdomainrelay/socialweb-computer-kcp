package pds

import (
	"context"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
)

type Sequencer struct {
	mu   sync.Mutex
	subs map[chan *repo.CommitEvent]struct{}
}

func NewSequencer() *Sequencer {
	return &Sequencer{subs: make(map[chan *repo.CommitEvent]struct{})}
}

func (s *Sequencer) OnCommit(_ context.Context, ev *repo.CommitEvent) error {
	if ev == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
	return nil
}

func (s *Sequencer) Subscribe() (<-chan *repo.CommitEvent, func()) {
	ch := make(chan *repo.CommitEvent, 512)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}

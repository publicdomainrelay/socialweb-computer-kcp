package relay

import (
	"sync"
)

const (
	DefaultMaxBacklog      = 10000
	DefaultMaxBacklogBytes = 64 * 1024 * 1024
)

type Sequencer struct {
	mu       sync.Mutex
	seq      int64
	max      int
	maxBytes int
	bytes    int
	backlog  []Frame
	sizes    []int
	subs     map[uint64]chan Frame
	nextSub  uint64
}

func NewSequencer(maxBacklog int) *Sequencer {
	if maxBacklog <= 0 {
		maxBacklog = DefaultMaxBacklog
	}
	return &Sequencer{
		max:      maxBacklog,
		maxBytes: DefaultMaxBacklogBytes,
		subs:     map[uint64]chan Frame{},
	}
}

func (s *Sequencer) Append(frame Frame) Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	frame.Seq = s.seq
	frame.Time = nowISO()
	size := len(frame.Blocks) + 512
	s.backlog = append(s.backlog, frame)
	s.sizes = append(s.sizes, size)
	s.bytes += size
	for len(s.backlog) > s.max || (s.bytes > s.maxBytes && len(s.backlog) > 1) {
		s.bytes -= s.sizes[0]
		s.backlog = s.backlog[1:]
		s.sizes = s.sizes[1:]
	}
	for id, ch := range s.subs {
		select {
		case ch <- frame:
		default:
			delete(s.subs, id)
			close(ch)
		}
	}
	return frame
}

func (s *Sequencer) Head() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *Sequencer) Watch(since int64) ([]Frame, <-chan Frame, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backlog := make([]Frame, 0, len(s.backlog))
	for _, f := range s.backlog {
		if f.Seq > since {
			backlog = append(backlog, f)
		}
	}
	id := s.nextSub
	s.nextSub++
	ch := make(chan Frame, 4096)
	s.subs[id] = ch
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if existing, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(existing)
		}
	}
	return backlog, ch, cancel
}

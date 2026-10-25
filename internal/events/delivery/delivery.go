package delivery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

type Phase string

const (
	PhasePending Phase = "Pending"

	PhaseRunning Phase = "Running"

	PhaseFailed Phase = "Failed"
)

const (
	ReasonStarting = "Starting"

	ReasonStreamOpen = "StreamOpen"

	ReasonSubscribeFailed = "SubscribeFailed"

	ReasonStreamClosed = "StreamClosed"

	ReasonDeliverFailed = "DeliverFailed"

	DefaultReconnectDelay = time.Second

	DefaultMaxReconnectDelay = 30 * time.Second

	RelayRewindEvents = 1000

	JetstreamRewindMicros = int64(30 * time.Second / time.Microsecond)

	ProgressDelay = time.Minute

	RecentResumeWindow = 10 * time.Minute

	StableConnection = 30 * time.Second

	gaplessPrefix = "jetstream/"
)

type State struct {
	Phase Phase

	Ready bool

	Failed bool

	Reason string

	Message string

	Started bool

	Cursor int64

	Lag int64

	LastEventAt time.Time

	ServerFilter *events.ServerFilter
}

type Config struct {
	Filter events.Filter

	ReconnectDelay time.Duration

	MaxReconnectDelay time.Duration

	Rewind int64

	Live bool
}

type Deps struct {
	Source events.Source

	Sink events.Sink

	Resume func() *int64

	Changed func()

	Progress func()
}

type Loop struct {
	cfg Config

	deps Deps

	mu sync.Mutex

	state State
}

func New(cfg Config, deps Deps) *Loop {
	cfg.ReconnectDelay = opt.Positive(cfg.ReconnectDelay, DefaultReconnectDelay)
	cfg.MaxReconnectDelay = opt.Positive(cfg.MaxReconnectDelay, DefaultMaxReconnectDelay)
	return &Loop{cfg: cfg, deps: deps}
}

func (l *Loop) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state
	if s.Phase == "" {
		s.Phase, s.Reason, s.Message = PhasePending, ReasonStarting, "the stream is starting"
	}
	return s
}

func (l *Loop) changed() {
	if l.deps.Changed != nil {
		l.deps.Changed()
	}
}

func (l *Loop) progressed() {
	if l.deps.Progress == nil {
		l.changed()
		return
	}
	l.deps.Progress()
}

func (l *Loop) set(phase Phase, ready, failed bool, reason, message string) {
	l.mu.Lock()
	l.state.Phase, l.state.Ready, l.state.Failed, l.state.Reason, l.state.Message = phase, ready, failed, reason, message
	l.mu.Unlock()
	l.changed()
}

func (l *Loop) cursor() *int64 {
	l.mu.Lock()
	started, cursor := l.state.Started, l.state.Cursor
	l.mu.Unlock()
	if started && cursor <= 0 && l.cfg.Live {
		return nil
	}
	if started {
		return Rewound(cursor, l.cfg.Rewind)
	}
	if l.deps.Resume != nil {
		return l.deps.Resume()
	}
	return nil
}

func Recent(savedAt, now time.Time) bool {
	return !savedAt.IsZero() && now.Sub(savedAt) < RecentResumeWindow
}

func atLeast(cursor, floor *int64) *int64 {
	if cursor == nil || floor == nil || *cursor <= 0 || *cursor >= *floor {
		return cursor
	}
	v := *floor
	return &v
}

func Rewound(cursor, rewind int64) *int64 {
	if cursor > 0 && rewind > 0 {
		cursor = max(cursor-rewind, 1)
	}
	return &cursor
}

func (l *Loop) Run(ctx context.Context) {
	base, limit := l.cfg.ReconnectDelay, l.cfg.MaxReconnectDelay
	delay := base
	var floor *int64
	for ctx.Err() == nil {
		filter := l.cfg.Filter
		filter.Cursor = atLeast(l.cursor(), floor)
		floor = filter.Cursor
		upstream, err := l.deps.Source.Subscribe(ctx, filter)
		if err != nil {
			l.set(PhaseFailed, false, true, ReasonSubscribeFailed, err.Error())
		} else {
			l.mu.Lock()
			l.state.ServerFilter = nil
			l.mu.Unlock()
			l.set(PhaseRunning, true, false, ReasonStreamOpen, "")
			opened := time.Now()
			if l.consume(ctx, upstream) && time.Since(opened) >= StableConnection {
				delay = base
			}
			_ = upstream.Close()
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, limit)
	}
}

func (l *Loop) consume(ctx context.Context, upstream events.Stream) bool {
	received := false
	for {
		event, err := upstream.Next(ctx)
		if err != nil {
			if errors.Is(err, events.ErrFutureCursor) {
				l.mu.Lock()
				l.state.Started, l.state.Cursor = true, 0
				l.mu.Unlock()
			}
			if ctx.Err() == nil {
				l.set(PhaseFailed, false, true, ReasonStreamClosed, err.Error())
			}
			return received
		}
		if event.ServerFilter != nil {
			l.mu.Lock()
			l.state.ServerFilter = event.ServerFilter
			l.mu.Unlock()
			l.changed()
			continue
		}
		first := !received
		received = true
		l.observe(event)
		if len(event.Ops) == 0 {
			if first {
				l.changed()
			} else {
				l.progressed()
			}
			continue
		}
		if err := l.deps.Sink.Deliver(ctx, event); err != nil {
			if ctx.Err() != nil {
				return received
			}
			l.set(PhaseFailed, true, true, ReasonDeliverFailed, err.Error())
			continue
		}
		l.mu.Lock()
		recovered := l.state.Failed && l.state.Reason == ReasonDeliverFailed
		l.mu.Unlock()
		if recovered {
			l.set(PhaseRunning, true, false, ReasonStreamOpen, "")
		} else {
			l.changed()
		}
	}
}

func (l *Loop) observe(e events.Event) {
	at := e.Time
	if at.IsZero() {
		at = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state.Lag = 0
	if !strings.HasPrefix(e.Source, gaplessPrefix) && !l.state.ServerFilter.Gapless() && l.state.Started && e.Seq > l.state.Cursor+1 {
		l.state.Lag = e.Seq - l.state.Cursor - 1
	}
	l.state.Started, l.state.Cursor, l.state.LastEventAt = true, e.Seq, at
}

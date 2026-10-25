package delivery

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

type Snapshot struct {
	Started bool

	Cursor int64

	LastEventAt time.Time

	Lag int64

	Err error
}

type TrackerConfig struct {
	CountGaps bool

	Seed int64
}

type Tracker struct {
	gaps bool

	mu sync.Mutex

	s Snapshot
}

func NewTracker(cfg TrackerConfig) *Tracker {
	t := &Tracker{gaps: cfg.CountGaps}
	if cfg.Seed > 0 {
		t.s.Cursor = cfg.Seed
	}
	return t
}

func (t *Tracker) Observe(e events.Event, gapless bool) bool {
	at := e.Time
	if at.IsZero() {
		at = time.Now().UTC()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	transition := !t.s.Started || t.s.Err != nil
	t.s.Lag = 0
	if t.gaps && !gapless && t.s.Started && e.Seq > t.s.Cursor+1 {
		t.s.Lag = e.Seq - t.s.Cursor - 1
	}
	t.s.Started, t.s.Cursor, t.s.LastEventAt, t.s.Err = true, e.Seq, at, nil
	return transition
}

func (t *Tracker) Fail(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.Err = err
}

func (t *Tracker) Recover() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.Err = nil
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.s
}

type Tracked struct {
	stream events.Stream

	tracker *Tracker

	changed func()

	progress func()

	gapless bool
}

var _ events.Stream = (*Tracked)(nil)

func NewTracked(stream events.Stream, tracker *Tracker, changed, progress func()) *Tracked {
	if progress == nil {
		progress = changed
	}
	return &Tracked{stream: stream, tracker: tracker, changed: changed, progress: progress}
}

func (t *Tracked) Next(ctx context.Context) (events.Event, error) {
	event, err := t.stream.Next(ctx)
	if err != nil {
		if ctx.Err() == nil {
			t.tracker.Fail(err)
			t.changed()
		}
		return event, err
	}
	if event.ServerFilter != nil {
		t.gapless = event.ServerFilter.Gapless()
		return event, nil
	}
	if t.tracker.Observe(event, t.gapless) || len(event.Ops) > 0 {
		t.changed()
	} else {
		t.progress()
	}
	return event, nil
}

func (t *Tracked) Close() error {
	return t.stream.Close()
}

type CursorStore struct {
	dir string
}

type cursorFile struct {
	UID string `json:"uid"`

	Cursor int64 `json:"cursor"`
}

func NewCursorStore(dir string) CursorStore {
	return CursorStore{dir: strings.TrimSpace(dir)}
}

func (s CursorStore) path(workspace, name string) string {
	clean := func(v string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
				return r
			default:
				return '_'
			}
		}, strings.TrimSpace(v))
	}
	return filepath.Join(s.dir, clean(workspace), clean(name)+".json")
}

func (s CursorStore) Load(workspace, name, uid string) (int64, bool) {
	if s.dir == "" {
		return 0, false
	}
	var f cursorFile
	if err := fsx.ReadJSON(s.path(workspace, name), &f); err != nil || f.UID != uid {
		return 0, false
	}
	return f.Cursor, true
}

func (s CursorStore) Save(workspace, name, uid string, cursor int64) error {
	if s.dir == "" {
		return nil
	}
	return fsx.WriteJSON(s.path(workspace, name), cursorFile{UID: uid, Cursor: cursor}, 0o600)
}

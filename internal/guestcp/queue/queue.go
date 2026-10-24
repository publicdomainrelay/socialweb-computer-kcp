package queue

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
)

const (
	Dir = "guest-events"

	MaxPending = 256

	MaxRecordBytes = 32 << 10

	fileSuffix = ".ndjson"
)

var computeVMName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

type Hub struct {
	mu sync.Mutex

	dir string

	vms map[string]*vmQueue

	seen map[string]bool

	retained bool

	publish guestcp.Publish

	publisher int
}

type vmQueue struct {
	path string

	entries []guestcp.QueuedEvent

	keys map[string]bool

	last int64

	wake chan struct{}

	gone bool
}

var (
	_ guestcp.Events = (*Hub)(nil)

	_ guestcp.Queue = (*Hub)(nil)
)

func New() *Hub {
	return &Hub{vms: map[string]*vmQueue{}, seen: map[string]bool{}}
}

func (h *Hub) Open(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("guestcp: the guest event queue names no state dir")
	}
	dir = filepath.Join(dir, Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("guestcp: creating the guest event queue dir: %w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dir == dir {
		return nil
	}
	h.dir, h.vms, h.seen, h.retained = dir, map[string]*vmQueue{}, map[string]bool{}, false
	return nil
}

func (h *Hub) Append(_ context.Context, computeVM string, event guestcp.QueuedEvent) (bool, error) {
	if event.URI == "" || event.CID == "" || len(event.Record) == 0 {
		return false, errors.New("guestcp: a queued guest event needs a uri, a cid and a record")
	}
	if len(event.Record) > MaxRecordBytes {
		return false, fmt.Errorf("guestcp: the guest event record is larger than %d bytes", MaxRecordBytes)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, event.Record); err != nil {
		return false, fmt.Errorf("guestcp: the guest event record is not JSON: %w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	q, err := h.queueLocked(computeVM)
	if err != nil {
		return false, err
	}
	key := event.URI + "#" + event.CID
	if q.keys[key] {
		return false, nil
	}
	if q.pending() >= MaxPending {
		return false, guestcp.ErrQueueFull
	}
	entry := guestcp.QueuedEvent{Seq: q.last + 1, URI: event.URI, CID: event.CID, Record: compact.Bytes()}
	line, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}
	if err := appendLine(q.path, line); err != nil {
		return false, err
	}
	q.entries = append(q.entries, entry)
	q.keys[key] = true
	q.last = entry.Seq
	q.notify()
	return true, nil
}

func (h *Hub) Serve(publish guestcp.Publish) func() {
	h.mu.Lock()
	h.publisher++
	id := h.publisher
	h.publish = publish
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		if h.publisher == id {
			h.publish = nil
		}
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(ctx context.Context, event guestcp.GuestEvent) error {
	h.mu.Lock()
	publish := h.publish
	h.mu.Unlock()
	if publish == nil {
		return guestcp.ErrNoPublisher
	}
	return publish(ctx, event)
}

func (h *Hub) Subscribe(computeVM string, cursor *int64) (guestcp.EventStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	q, err := h.queueLocked(computeVM)
	if err != nil {
		return nil, err
	}
	after := int64(0)
	if cursor != nil {
		if *cursor > q.last {
			return nil, fmt.Errorf("%w: cursor %d, last seq %d", guestcp.ErrFutureCursor, *cursor, q.last)
		}
		if err := q.prune(*cursor); err != nil {
			return nil, err
		}
		after = *cursor
	}
	return &stream{hub: h, q: q, after: after, closed: make(chan struct{})}, nil
}

type stream struct {
	hub *Hub

	q *vmQueue

	after int64

	once sync.Once

	closed chan struct{}
}

func (s *stream) Next(ctx context.Context) (guestcp.QueuedEvent, error) {
	for {
		s.hub.mu.Lock()
		if s.q.gone {
			s.hub.mu.Unlock()
			return guestcp.QueuedEvent{}, guestcp.ErrQueueGone
		}
		for _, entry := range s.q.entries {
			if entry.Seq > s.after && len(entry.Record) > 0 {
				s.after = entry.Seq
				s.hub.mu.Unlock()
				return entry, nil
			}
		}
		wake := s.q.wake
		s.hub.mu.Unlock()
		select {
		case <-ctx.Done():
			return guestcp.QueuedEvent{}, ctx.Err()
		case <-s.closed:
			return guestcp.QueuedEvent{}, guestcp.ErrQueueGone
		case <-wake:
		}
	}
}

func (s *stream) Close() {
	s.once.Do(func() { close(s.closed) })
}

func (q *vmQueue) prune(through int64) error {
	changed := false
	for i := range q.entries {
		if q.entries[i].Seq <= through && len(q.entries[i].Record) > 0 {
			q.entries[i].Record = nil
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return q.rewrite()
}

func (h *Hub) Retain(computeVMs []string) {
	live := make(map[string]bool, len(computeVMs))
	for _, name := range computeVMs {
		live[name] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dir == "" {
		return
	}
	stale := map[string]bool{}
	if !h.retained {
		for _, name := range h.onDisk() {
			stale[name] = true
		}
		h.retained = true
	}
	for name := range h.seen {
		stale[name] = true
	}
	for name := range stale {
		if !live[name] {
			h.dropLocked(name)
		}
	}
	h.seen = live
}

func (h *Hub) onDisk() []string {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), fileSuffix); ok && !entry.IsDir() {
			out = append(out, name)
		}
	}
	return out
}

func (h *Hub) dropLocked(name string) {
	if q := h.vms[name]; q != nil {
		q.gone = true
		q.notify()
		delete(h.vms, name)
	}
	if computeVMName.MatchString(name) {
		_ = os.Remove(filepath.Join(h.dir, name+fileSuffix))
		syncDir(h.dir)
	}
}

func (h *Hub) queueLocked(computeVM string) (*vmQueue, error) {
	if h.dir == "" {
		return nil, guestcp.ErrQueueNotOpen
	}
	if !computeVMName.MatchString(computeVM) {
		return nil, fmt.Errorf("guestcp: %q is not a ComputeVM name", computeVM)
	}
	if q := h.vms[computeVM]; q != nil {
		return q, nil
	}
	q, err := load(filepath.Join(h.dir, computeVM+fileSuffix))
	if err != nil {
		return nil, err
	}
	h.vms[computeVM] = q
	return q, nil
}

func load(path string) (*vmQueue, error) {
	q := &vmQueue{path: path, keys: map[string]bool{}, wake: make(chan struct{})}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, fmt.Errorf("guestcp: reading the guest event queue: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 2*MaxRecordBytes)
	torn := false
	for scanner.Scan() {
		var entry guestcp.QueuedEvent
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.Seq <= q.last || entry.URI == "" {
			torn = true
			continue
		}
		q.entries = append(q.entries, entry)
		q.keys[entry.URI+"#"+entry.CID] = true
		q.last = entry.Seq
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("guestcp: reading the guest event queue: %w", err)
	}
	if torn {
		if err := q.rewrite(); err != nil {
			return nil, err
		}
	}
	return q, nil
}

func (q *vmQueue) pending() int {
	n := 0
	for _, entry := range q.entries {
		if len(entry.Record) > 0 {
			n++
		}
	}
	return n
}

func (q *vmQueue) notify() {
	close(q.wake)
	q.wake = make(chan struct{})
}

func (q *vmQueue) rewrite() error {
	var buf bytes.Buffer
	for _, entry := range q.entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := q.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("guestcp: rewriting the guest event queue: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("guestcp: rewriting the guest event queue: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("guestcp: rewriting the guest event queue: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, q.path); err != nil {
		return fmt.Errorf("guestcp: rewriting the guest event queue: %w", err)
	}
	syncDir(filepath.Dir(q.path))
	return nil
}

func appendLine(path string, line []byte) error {
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("guestcp: appending to the guest event queue: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("guestcp: appending to the guest event queue: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("guestcp: syncing the guest event queue: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		syncDir(filepath.Dir(path))
	}
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

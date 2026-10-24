package fswatch

import (
	"errors"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

var ErrClosed = errors.New("fswatch: the watch is closed")

type Watcher struct {
	mu     sync.Mutex
	w      *fsnotify.Watcher
	refs   map[string]int
	subs   map[*Sub]struct{}
	closed bool
}

func New() *Watcher {
	return &Watcher{refs: map[string]int{}, subs: map[*Sub]struct{}{}}
}

var shared = New()

func Shared() *Watcher { return shared }

type item struct {
	ev  fsnotify.Event
	err error
}

type Sub struct {
	parent  *Watcher
	paths   map[string]bool
	events  chan fsnotify.Event
	errors  chan error
	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	queue   []item
	pending map[fsnotify.Event]bool
	failed  bool
}

func (w *Watcher) Subscribe(paths ...string) (*Sub, error) {
	s := &Sub{parent: w, paths: map[string]bool{}, events: make(chan fsnotify.Event), errors: make(chan error),
		wake: make(chan struct{}, 1), done: make(chan struct{}), pending: map[fsnotify.Event]bool{}}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	w.subs[s] = struct{}{}
	w.mu.Unlock()
	go s.pump()
	for _, p := range paths {
		if err := s.Add(p); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (w *Watcher) open() (*fsnotify.Watcher, error) {
	if w.w != nil {
		return w.w, nil
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w.w = fw
	go w.run(fw)
	return fw, nil
}

func (w *Watcher) run(fw *fsnotify.Watcher) {
	for {
		select {
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			w.dispatch(ev)
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			w.broadcast(err)
		}
	}
}

func (w *Watcher) dispatch(ev fsnotify.Event) {
	name := filepath.Clean(ev.Name)
	dir := filepath.Dir(name)
	w.mu.Lock()
	defer w.mu.Unlock()
	for s := range w.subs {
		if s.paths[name] || s.paths[dir] {
			s.enqueue(item{ev: ev})
		}
	}
}

func (w *Watcher) broadcast(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for s := range w.subs {
		s.enqueue(item{err: err})
	}
}

func (w *Watcher) release(path string) {
	w.refs[path]--
	if w.refs[path] > 0 {
		return
	}
	delete(w.refs, path)
	if w.w != nil {
		_ = w.w.Remove(path)
	}
}

func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	subs := make([]*Sub, 0, len(w.subs))
	for s := range w.subs {
		subs = append(subs, s)
	}
	fw := w.w
	w.w = nil
	w.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
	if fw == nil {
		return nil
	}
	return fw.Close()
}

func (s *Sub) Events() <-chan fsnotify.Event { return s.events }

func (s *Sub) Errors() <-chan error { return s.errors }

func (s *Sub) Add(path string) error {
	path = filepath.Clean(path)
	w := s.parent
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, live := w.subs[s]; !live || w.closed {
		return ErrClosed
	}
	if s.paths[path] {
		return nil
	}
	fw, err := w.open()
	if err != nil {
		return err
	}
	if err := fw.Add(path); err != nil {
		return err
	}
	s.paths[path] = true
	w.refs[path]++
	return nil
}

func (s *Sub) Remove(path string) {
	path = filepath.Clean(path)
	w := s.parent
	w.mu.Lock()
	defer w.mu.Unlock()
	if !s.paths[path] {
		return
	}
	delete(s.paths, path)
	w.release(path)
}

func (s *Sub) Close() {
	s.once.Do(func() {
		w := s.parent
		w.mu.Lock()
		delete(w.subs, s)
		for p := range s.paths {
			w.release(p)
		}
		s.paths = map[string]bool{}
		w.mu.Unlock()
		close(s.done)
	})
}

func (s *Sub) enqueue(it item) {
	s.mu.Lock()
	switch {
	case it.err != nil && s.failed:
	case it.err != nil:
		s.failed = true
		s.queue = append(s.queue, it)
	case s.pending[it.ev]:
	default:
		s.pending[it.ev] = true
		s.queue = append(s.queue, it)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sub) pump() {
	defer close(s.errors)
	defer close(s.events)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.done:
				return
			}
		}
		it := s.queue[0]
		s.queue = s.queue[1:]
		if len(s.queue) == 0 {
			s.queue = nil
		}
		if it.err != nil {
			s.failed = false
		} else {
			delete(s.pending, it.ev)
		}
		s.mu.Unlock()
		if it.err != nil {
			select {
			case s.errors <- it.err:
			case <-s.done:
				return
			}
			continue
		}
		select {
		case s.events <- it.ev:
		case <-s.done:
			return
		}
	}
}

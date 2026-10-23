package apiserverfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultDialTimeout = 5 * time.Second

const DefaultIdleTimeout = 5 * time.Minute

var ErrCurrentBackend = errors.New("apiserverfront: refusing to drain the current backend")

type Status struct {
	Address      string         `json:"address"`
	Backend      string         `json:"backend"`
	Generation   int64          `json:"generation"`
	Open         map[string]int `json:"open"`
	Accepted     int64          `json:"accepted"`
	DialFailures int64          `json:"dialFailures"`
	IdleClosed   int64          `json:"idleClosed"`
	Inherited    bool           `json:"inherited"`
	PID          int            `json:"pid"`
	SwitchedAt   time.Time      `json:"switchedAt,omitzero"`
	Holds        map[string]int `json:"holds,omitempty"`
}

type State struct {
	Address    string         `json:"address,omitempty"`
	Backend    string         `json:"backend"`
	Generation int64          `json:"generation"`
	SwitchedAt time.Time      `json:"switchedAt"`
	Holds      map[string]int `json:"holds,omitempty"`
}

type Options struct {
	StatePath   string
	DialTimeout time.Duration
	IdleTimeout time.Duration
	Inherited   bool
	Holds       map[string]*Held
	FDStore     string
	Log         *slog.Logger
}

type Held struct {
	Name string
	Port int
	f    *os.File
}

func (h *Held) Close() error {
	if h == nil || h.f == nil {
		return nil
	}
	return h.f.Close()
}

var ErrHoldName = errors.New("apiserverfront: a hold name is 1 to 64 bytes of [a-z0-9.@-]")

func ValidHoldName(name string) error {
	if name == "" || len(name) > 64 {
		return ErrHoldName
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '@') {
			return ErrHoldName
		}
	}
	return nil
}

type pair struct {
	backend string
	client  net.Conn
	server  net.Conn
}

type Front struct {
	ln       net.Listener
	o        Options
	log      *slog.Logger
	mu       sync.Mutex
	state    State
	conns    map[*pair]struct{}
	holds    map[string]*Held
	accepted atomic.Int64
	failures atomic.Int64
	idle     atomic.Int64
	wg       sync.WaitGroup
}

func ValidBackend(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("apiserverfront: backend %q is no host:port: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("apiserverfront: backend %q names no host", addr)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("apiserverfront: backend %q names no port", addr)
	}
	return nil
}

func New(ln net.Listener, o Options) (*Front, error) {
	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	f := &Front{ln: ln, o: o, log: log, conns: map[*pair]struct{}{}, holds: map[string]*Held{}}
	maps.Copy(f.holds, o.Holds)
	if o.StatePath != "" {
		b, err := os.ReadFile(o.StatePath)
		switch {
		case err == nil:
			var st State
			if err := json.Unmarshal(b, &st); err != nil {
				return nil, fmt.Errorf("apiserverfront: %s: %w", o.StatePath, err)
			}
			if st.Backend != "" {
				if err := ValidBackend(st.Backend); err != nil {
					return nil, err
				}
			}
			f.state = st
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
		if a := ln.Addr().String(); f.state.Address != a {
			f.state.Address = a
			if err := f.persist(f.state); err != nil {
				return nil, err
			}
		}
	}
	if err := f.restoreHolds(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Front) restoreHolds() error {
	want := map[string]int{}
	maps.Copy(want, f.state.Holds)
	for name, h := range f.holds {
		want[name] = h.Port
	}
	for name, port := range f.state.Holds {
		if _, ok := f.holds[name]; ok {
			continue
		}
		h, err := f.newHold(name, port)
		if err != nil {
			f.log.Warn("a recorded hold is taken; it is dropped and the next hold of it binds a new port",
				"hold", name, "port", port, "err", err.Error())
			delete(want, name)
			continue
		}
		f.log.Info("hold restored on its recorded port", "hold", name, "port", h.Port)
	}
	if maps.Equal(want, f.state.Holds) {
		return nil
	}
	f.state.Holds = want
	return f.persist(f.state)
}

func (f *Front) newHold(name string, port int) (*Held, error) {
	h, err := bindHold(name, port)
	if err != nil {
		return nil, err
	}
	if f.o.FDStore != "" {
		if err := storeHold(f.o.FDStore, h); err != nil {
			h.Close()
			return nil, err
		}
	}
	f.holds[name] = h
	return h, nil
}

func (f *Front) Hold(name string) (int, error) {
	if err := ValidHoldName(name); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.holds[name]; ok {
		return h.Port, nil
	}
	h, err := f.newHold(name, 0)
	if err != nil {
		return 0, err
	}
	next := f.state
	next.Holds = maps.Clone(f.state.Holds)
	if next.Holds == nil {
		next.Holds = map[string]int{}
	}
	next.Holds[name] = h.Port
	if err := f.persist(next); err != nil {
		return 0, fmt.Errorf("apiserverfront: recording hold %q: %w", name, err)
	}
	f.state = next
	f.log.Info("port held", "hold", name, "port", h.Port, "fdStore", f.o.FDStore != "")
	return h.Port, nil
}

func (f *Front) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { f.ln.Close() })
	defer stop()
	st := f.Status()
	f.log.Info("front serving", "address", st.Address, "backend", st.Backend, "generation", st.Generation,
		"inherited", st.Inherited, "pid", st.PID)
	var err error
	for {
		var c net.Conn
		c, err = f.ln.Accept()
		if err != nil {
			break
		}
		f.accepted.Add(1)
		f.wg.Go(func() {
			f.handle(c)
		})
	}
	f.closeAll()
	f.wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (f *Front) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p := range f.conns {
		p.client.Close()
		p.server.Close()
	}
}

func (f *Front) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Backend
}

func (f *Front) handle(c net.Conn) {
	backend := f.current()
	if backend == "" {
		f.failures.Add(1)
		c.Close()
		return
	}
	s, err := net.DialTimeout("tcp", backend, f.o.DialTimeout)
	if err != nil {
		f.failures.Add(1)
		f.log.Warn("backend dial failed; the front does not fail over", "backend", backend, "err", err.Error())
		c.Close()
		return
	}
	p := &pair{backend: backend, client: c, server: s}
	f.mu.Lock()
	f.conns[p] = struct{}{}
	f.mu.Unlock()
	done := make(chan struct{}, 2)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var idled atomic.Bool
	idle := f.o.IdleTimeout
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err == nil {
				continue
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && !idled.Load() {
				if time.Since(time.Unix(0, last.Load())) < idle {
					continue
				}
				idled.Store(true)
				c.Close()
				s.Close()
			}
			break
		}
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(s, c)
	go pipe(c, s)
	<-done
	<-done
	if idled.Load() {
		f.idle.Add(1)
	}
	c.Close()
	s.Close()
	f.mu.Lock()
	delete(f.conns, p)
	f.mu.Unlock()
}

func (f *Front) persist(st State) error {
	if f.o.StatePath == "" {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.o.StatePath), 0o700); err != nil {
		return err
	}
	tmp := f.o.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.o.StatePath)
}

func (f *Front) SetBackend(addr string) (Status, error) {
	if err := ValidBackend(addr); err != nil {
		return f.Status(), err
	}
	f.mu.Lock()
	next := f.state
	if next.Backend != addr {
		next = State{Address: f.state.Address, Backend: addr, Generation: f.state.Generation + 1, SwitchedAt: time.Now().UTC(),
			Holds: f.state.Holds}
		if err := f.persist(next); err != nil {
			f.mu.Unlock()
			return f.Status(), fmt.Errorf("apiserverfront: persisting the backend before the switch: %w", err)
		}
		f.log.Info("backend switched", "from", f.state.Backend, "to", addr, "generation", next.Generation)
		f.state = next
	}
	f.mu.Unlock()
	return f.Status(), nil
}

func (f *Front) Drain(backend string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if backend == f.state.Backend {
		return 0, fmt.Errorf("%w %s", ErrCurrentBackend, backend)
	}
	n := 0
	for p := range f.conns {
		if p.backend == backend {
			p.client.Close()
			p.server.Close()
			n++
		}
	}
	if n > 0 {
		f.log.Info("drained", "backend", backend, "closed", n)
	}
	return n, nil
}

func (f *Front) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	open := map[string]int{}
	for p := range f.conns {
		open[p.backend]++
	}
	var holds map[string]int
	if len(f.holds) > 0 {
		holds = make(map[string]int, len(f.holds))
		for name, h := range f.holds {
			holds[name] = h.Port
		}
	}
	return Status{Address: f.ln.Addr().String(), Backend: f.state.Backend, Generation: f.state.Generation, Open: open,
		Accepted: f.accepted.Load(), DialFailures: f.failures.Load(), IdleClosed: f.idle.Load(), Inherited: f.o.Inherited, PID: os.Getpid(),
		SwitchedAt: f.state.SwitchedAt, Holds: holds}
}

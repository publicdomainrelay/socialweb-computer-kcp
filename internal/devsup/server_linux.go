//go:build linux

package devsup

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

type unit struct {
	spec        hostsvc.UnitSpec
	st          hostsvc.UnitStatus
	pgid        int
	leaderTicks uint64
	order       int
	stopping    bool
	timedOut    bool
	exited      chan struct{}
	restarts    []time.Time
	restartT    *time.Timer
	store       *fdStore
}

type watcher struct {
	prefix string
	ch     chan hostsvc.UnitEvent
}

type spawnReq struct {
	argv  []string
	attr  *os.ProcAttr
	reply chan spawnResult
}

type spawnResult struct {
	pid int
	err error
}

type server struct {
	o        Options
	log      *slog.Logger
	id       string
	boot     string
	hello    hostsvc.ProcHello
	mu       sync.Mutex
	units    map[string]*unit
	seq      int
	watchers map[*watcher]struct{}
	spawn    chan spawnReq
	done     chan struct{}
	life     context.Context
	end      context.CancelFunc
	stopOnce sync.Once
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

var errStopping = errors.New("devsup: devsup is stopping")

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func Run(ctx context.Context, o Options) error {
	if o.State == "" || o.Socket == "" {
		return errors.New("devsup: --state and --socket are required")
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	log = log.With("component", "devsup")
	if err := os.MkdirAll(filepath.Join(o.State, "units"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.Socket), 0o700); err != nil {
		return err
	}
	lockf, err := os.OpenFile(filepath.Join(o.State, "devsup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockf.Close()
	if err := hostos.Lock(lockf, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		return fmt.Errorf("devsup: another devsup holds %s: %w", lockf.Name(), err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("devsup: PR_SET_CHILD_SUBREAPER: %w", err)
	}
	boot, err := hostos.BootID()
	if err != nil {
		return err
	}
	s := &server{o: o, log: log, id: newID(), boot: boot, units: map[string]*unit{},
		watchers: map[*watcher]struct{}{}, spawn: make(chan spawnReq), done: make(chan struct{}), conns: map[net.Conn]struct{}{}}
	s.life, s.end = context.WithCancel(context.WithoutCancel(ctx))
	defer s.end()
	ticks, _ := hostos.ProcStart(os.Getpid())
	s.hello = hostsvc.ProcHello{Instance: s.id, PID: os.Getpid(), StartTicks: ticks, BootID: boot,
		Started: time.Now().UTC(), State: o.State, Socket: o.Socket}
	s.sweep()
	abort := func(err error) error {
		s.stopOnce.Do(s.finish)
		s.wg.Wait()
		return err
	}
	s.wg.Go(s.spawner)
	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)
	s.wg.Go(func() { s.reaper(sigchld) })
	os.Remove(o.Socket)
	l, err := net.Listen("unix", o.Socket)
	if err != nil {
		return abort(fmt.Errorf("devsup: listening on %s: %w", o.Socket, err))
	}
	os.Chmod(o.Socket, 0o600)
	if err := fsx.WriteJSON(filepath.Join(o.State, "devsup.json"), s.hello, 0o600); err != nil {
		l.Close()
		return abort(err)
	}
	log.Info("devsup listening", "instance", s.id, "pid", os.Getpid(), "socket", o.Socket, "state", o.State,
		"dieWith", o.DieWith, "label", o.Instance)
	s.wg.Go(s.orphanKiller)
	term := make(chan os.Signal, 2)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	wake := [2]int{-1, -1}
	if o.DieWith > 0 {
		if err := unix.Pipe2(wake[:], unix.O_CLOEXEC); err != nil {
			l.Close()
			return abort(fmt.Errorf("devsup: the die-with wake pipe: %w", err))
		}
		s.wg.Go(func() { s.dieWith(o.DieWith, wake[0]) })
	}
	s.wg.Go(func() {
		select {
		case sig := <-term:
			log.Info("devsup stopping on a signal", "signal", sig.String())
			s.shutdown()
		case <-ctx.Done():
			s.shutdown()
		case <-s.done:
		}
	})
	s.wg.Go(func() { s.accept(l) })
	<-s.done
	l.Close()
	s.closeConns()
	if wake[1] >= 0 {
		unix.Close(wake[1])
	}
	s.wg.Wait()
	if wake[0] >= 0 {
		unix.Close(wake[0])
	}
	os.Remove(o.Socket)
	os.Remove(filepath.Join(o.State, "devsup.json"))
	log.Info("devsup stopped", "instance", s.id)
	return nil
}

func (s *server) shutdown() {
	s.stopOnce.Do(func() {
		s.wg.Go(func() {
			s.stopAll()
			s.finish()
		})
	})
}

func (s *server) finish() {
	close(s.done)
	s.end()
}

func (s *server) stopAll() {
	s.mu.Lock()
	var names []*unit
	for _, u := range s.units {
		names = append(names, u)
	}
	s.mu.Unlock()
	sort.Slice(names, func(i, j int) bool { return names[i].order > names[j].order })
	for _, u := range names {
		if err := s.stop(u.spec.Name); err != nil {
			s.log.Error("stopping a unit at shutdown", "unit", u.spec.Name, "err", err.Error())
		}
		s.mu.Lock()
		if cur, ok := s.units[u.spec.Name]; ok && cur == u && u.st.State != hostsvc.Active {
			s.collect(u)
		}
		s.mu.Unlock()
	}
	for pid := range attributedTo(s.id) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (s *server) dieWith(pid, wake int) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		s.log.Info("the watched pid is already gone", "pid", pid)
		s.shutdown()
		return
	}
	defer unix.Close(fd)
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		if fds[0].Revents == 0 && fds[1].Revents != 0 {
			return
		}
		if n > 0 || err != nil {
			s.log.Info("the watched pid exited", "pid", pid)
			s.shutdown()
			return
		}
	}
}

func (s *server) spawner() {
	runtime.LockOSThread()
	for {
		var req spawnReq
		select {
		case <-s.done:
			return
		case req = <-s.spawn:
		}
		p, err := os.StartProcess(req.argv[0], req.argv, req.attr)
		if err != nil {
			req.reply <- spawnResult{err: err}
			continue
		}
		pid := p.Pid
		p.Release()
		req.reply <- spawnResult{pid: pid}
	}
}

func (s *server) reaper(sigchld <-chan os.Signal) {
	for {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if err == unix.EINTR {
				continue
			}
			if pid <= 0 || err != nil {
				break
			}
			s.exited(pid, ws)
		}
		select {
		case <-s.done:
			return
		case <-sigchld:
		}
	}
}

func (s *server) exited(pid int, ws unix.WaitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var u *unit
	for _, cand := range s.units {
		if cand.st.MainPID == pid && cand.exited != nil {
			u = cand
			break
		}
	}
	if u == nil {
		return
	}
	code, status := int32(CodeExited), int32(0)
	switch {
	case ws.Exited():
		status = int32(ws.ExitStatus())
	case ws.Signaled():
		code = CodeKilled
		if ws.CoreDump() {
			code = CodeDumped
		}
		status = int32(ws.Signal())
	}
	result := "success"
	switch {
	case code == CodeExited && status != 0:
		result = "exit-code"
	case code != CodeExited && u.stopping && u.timedOut:
		result = "timeout"
	case code != CodeExited && u.stopping && syscall.Signal(status) == u.killSignal() && CleanSignal(u.killSignal()):
		result = "success"
	case code == CodeDumped:
		result = "core-dump"
	case code != CodeExited:
		result = "signal"
	}
	u.st.ExecMainCode, u.st.ExecMainStatus, u.st.Result = code, status, result
	s.log.Info("unit exited", "unit", u.spec.Name, "pid", pid, "result", result, "code", code, "status", status,
		"stopping", u.stopping, "ranFor", time.Since(u.st.ActiveSince).Round(time.Millisecond).String())
	u.st.MainPID = 0
	close(u.exited)
	u.exited = nil
	pgid, sig, grace := u.pgid, u.killSignal(), u.stopTimeout()
	inv := u.st.InvocationID
	name := u.spec.Name
	s.wg.Go(func() { s.killRemainder(name, inv, pgid, sig, grace) })
	if !u.stopping && s.shouldRestart(u, result) {
		u.st.State, u.st.SubState = hostsvc.Activating, "auto-restart"
		s.emit(u)
		u.restartT = time.AfterFunc(RestartDelay, func() { s.restart(name, u) })
		return
	}
	if result == "success" {
		u.st.State, u.st.SubState = hostsvc.Inactive, "dead"
		s.emit(u)
		s.collect(u)
		return
	}
	u.st.State, u.st.SubState = hostsvc.Failed, "failed"
	s.persist(u)
	s.emit(u)
	s.log.Info("unit failed", "unit", u.spec.Name, "result", result, "code", code, "status", status)
}

func (s *server) shouldRestart(u *unit, result string) bool {
	switch u.spec.Restart {
	case hostsvc.RestartAlways:
	case hostsvc.RestartOnFailure:
		if result == "success" {
			return false
		}
	default:
		return false
	}
	now := time.Now()
	var keep []time.Time
	for _, t := range u.restarts {
		if now.Sub(t) < StartLimitInterval {
			keep = append(keep, t)
		}
	}
	u.restarts = keep
	if len(keep) >= StartLimitBurst {
		u.st.Result = "start-limit-hit"
		return false
	}
	u.restarts = append(u.restarts, now)
	return true
}

func (s *server) restart(name string, u *unit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.units[name]; !ok || cur != u || u.stopping || u.st.SubState != "auto-restart" {
		return
	}
	if err := s.launch(u); err != nil {
		s.log.Error("restarting a unit", "unit", name, "err", err.Error())
		return
	}
	s.log.Info("unit restarted", "unit", name, "pid", u.st.MainPID, "invocation", u.st.InvocationID)
}

func (s *server) killRemainder(name, inv string, pgid int, sig syscall.Signal, grace time.Duration) {
	set := s.members(name, inv, pgid)
	if len(set) == 0 {
		return
	}
	for _, pid := range set {
		syscall.Kill(pid, sig)
	}
	if s.awaitGone(name, inv, pgid, time.Now().Add(grace)) {
		return
	}
	for _, pid := range s.members(name, inv, pgid) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (s *server) awaitGone(name, inv string, pgid int, deadline time.Time) bool {
	ctx, cancel := context.WithDeadline(s.life, deadline)
	defer cancel()
	for {
		set := s.members(name, inv, pgid)
		if len(set) == 0 {
			return true
		}
		for _, pid := range set {
			err := hostos.AwaitExit(ctx, pid, 0)
			if ctx.Err() != nil {
				return false
			}
			if err != nil {
				s.log.Warn("waiting for a unit process to exit; the stop waits out its grace", "unit", name, "pid", pid, "err", err.Error())
				<-ctx.Done()
				return false
			}
		}
	}
}

func (s *server) members(name, inv string, pgid int) []int {
	seen := map[int]bool{}
	var out []int
	add := func(pid int) {
		if pid > 0 && !seen[pid] && pid != os.Getpid() {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	for _, p := range procTable() {
		if pgid > 0 && p.pgid == pgid {
			add(p.pid)
			for _, c := range childrenOf(p.pid) {
				if env := procEnv(c, EnvDevsup, EnvUnit); env[EnvDevsup] == s.id && (env[EnvUnit] == "" || env[EnvUnit] == name) {
					add(c)
				}
			}
		}
	}
	for pid, a := range attributedTo(s.id) {
		if a.unit == name && (inv == "" || a.invocation == inv) {
			add(pid)
		}
	}
	return out
}

func (s *server) orphanKiller() {
	if OrphanScanInterval <= 0 {
		return
	}
	t := time.NewTicker(OrphanScanInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		attributed := attributedTo(s.id)
		s.mu.Lock()
		for pid, a := range attributed {
			u, ok := s.units[a.unit]
			if ok && u.exited != nil && u.st.InvocationID == a.invocation {
				continue
			}
			if ok && u.stopping {
				continue
			}
			syscall.Kill(pid, syscall.SIGKILL)
			s.log.Info("killed a process of an inactive unit", "pid", pid, "unit", a.unit, "invocation", a.invocation)
		}
		s.mu.Unlock()
	}
}

func (u *unit) killSignal() syscall.Signal {
	if u.spec.KillSignal != 0 {
		return u.spec.KillSignal
	}
	return syscall.SIGTERM
}

func (u *unit) stopTimeout() time.Duration {
	if u.spec.StopTimeout > 0 {
		return u.spec.StopTimeout
	}
	return DefaultStopTimeout
}

func (s *server) recordPath(name string) string {
	return filepath.Join(s.o.State, "units", name+".json")
}

func (s *server) persist(u *unit) {
	rec := Record{Spec: u.spec, Status: u.st, PGID: u.pgid, LeaderTicks: u.leaderTicks, BootID: s.boot,
		Devsup: s.id, Order: u.order}
	if err := fsx.WriteJSON(s.recordPath(u.spec.Name), rec, 0o600); err != nil {
		s.log.Error("writing a unit record", "unit", u.spec.Name, "err", err.Error())
	}
}

func (s *server) collect(u *unit) {
	if cur, ok := s.units[u.spec.Name]; ok && cur == u {
		delete(s.units, u.spec.Name)
	}
	u.store.release()
	u.store = nil
	os.Remove(s.recordPath(u.spec.Name))
}

func (s *server) emit(u *unit) {
	ev := hostsvc.UnitEvent{Name: u.spec.Name, Status: u.st}
	for w := range s.watchers {
		if strings.HasPrefix(u.spec.Name, w.prefix) {
			select {
			case w.ch <- ev:
			default:
			}
		}
	}
}

func (s *server) sweep() {
	entries, _ := filepath.Glob(filepath.Join(s.o.State, "units", "*.json"))
	if len(entries) == 0 {
		return
	}
	var table []ProcEntry
	for _, p := range procTable() {
		table = append(table, ProcEntry{PID: p.pid, PGID: p.pgid, Ticks: p.ticks})
	}
	for _, path := range entries {
		b, err := os.ReadFile(path)
		var rec Record
		if err == nil {
			err = json.Unmarshal(b, &rec)
		}
		if err != nil {
			s.log.Error("an unreadable unit record is dropped", "path", path, "err", fmt.Sprint(err))
			os.Remove(path)
			continue
		}
		verdict := SweepNothing
		if rec.BootID == s.boot {
			verdict = SweepDecision(rec, table)
			if verdict == SweepGroup {
				syscall.Kill(-rec.PGID, syscall.SIGKILL)
			}
			if rec.Devsup != "" {
				for pid, a := range attributedTo(rec.Devsup) {
					if a.unit == rec.Spec.Name {
						syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			}
		}
		s.log.Info("swept a unit of a previous devsup", "unit", rec.Spec.Name, "pgid", rec.PGID,
			"leaderTicks", rec.LeaderTicks, "previous", rec.Devsup, "verdict", verdict.String())
		os.Remove(path)
	}
}

func (s *server) accept(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !s.track(c) {
			c.Close()
			return
		}
		s.wg.Go(func() { s.serve(c) })
	}
}

func (s *server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *server) closeConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
	for _, u := range s.units {
		u.store.release()
		u.store = nil
	}
}

func (s *server) serve(c net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req hostsvc.ProcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		json.NewEncoder(c).Encode(hostsvc.ProcError(fmt.Errorf("devsup: bad request: %w", err)))
		return
	}
	if req.Op == hostsvc.ProcOpWatch {
		s.watch(c, req.Prefix)
		return
	}
	resp := s.handle(req)
	json.NewEncoder(c).Encode(resp)
	if req.Op == hostsvc.ProcOpShutdown {
		s.shutdown()
	}
}

func (s *server) watch(c net.Conn, prefix string) {
	w := &watcher{prefix: prefix, ch: make(chan hostsvc.UnitEvent, 256)}
	s.mu.Lock()
	s.watchers[w] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, w)
		s.mu.Unlock()
	}()
	enc := json.NewEncoder(c)
	if err := enc.Encode(hostsvc.ProcResponse{}); err != nil {
		return
	}
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, c)
		close(gone)
	}()
	defer func() {
		c.Close()
		<-gone
	}()
	for {
		select {
		case ev := <-w.ch:
			if err := enc.Encode(hostsvc.ProcResponse{Event: &ev}); err != nil {
				return
			}
		case <-gone:
			return
		case <-s.done:
			return
		}
	}
}

func (s *server) handle(req hostsvc.ProcRequest) hostsvc.ProcResponse {
	fail := func(err error) hostsvc.ProcResponse { return hostsvc.ProcError(err) }
	switch req.Op {
	case hostsvc.ProcOpHello, hostsvc.ProcOpShutdown:
		h := s.hello
		return hostsvc.ProcResponse{Hello: &h}
	case hostsvc.ProcOpStart:
		if req.Spec == nil {
			return fail(errors.New("devsup: start without a spec"))
		}
		st, err := s.start(*req.Spec)
		if err != nil {
			return hostsvc.ProcResponse{Err: err.Error(), Status: &st}
		}
		return hostsvc.ProcResponse{Status: &st}
	case hostsvc.ProcOpInspect:
		st := s.inspect(req.Name)
		return hostsvc.ProcResponse{Status: &st}
	case hostsvc.ProcOpStop:
		if err := s.stop(req.Name); err != nil {
			return fail(err)
		}
		return hostsvc.ProcResponse{}
	case hostsvc.ProcOpStopSlice:
		if err := s.stopSlice(req.Slice); err != nil {
			return fail(err)
		}
		return hostsvc.ProcResponse{}
	case hostsvc.ProcOpResetFailed:
		s.resetFailed(req.Name)
		return hostsvc.ProcResponse{}
	case hostsvc.ProcOpList:
		return hostsvc.ProcResponse{Statuses: s.list(req.Prefix)}
	case hostsvc.ProcOpIdentify:
		id, err := s.identify(req.Name, req.PID)
		if err != nil {
			return fail(err)
		}
		return hostsvc.ProcResponse{Identity: &id}
	case hostsvc.ProcOpCheck:
		if req.Identity == nil {
			return fail(errors.New("devsup: check without an identity"))
		}
		obs, err := s.check(*req.Identity)
		if err != nil {
			r := fail(err)
			r.Observation = &obs
			return r
		}
		return hostsvc.ProcResponse{Observation: &obs}
	}
	return fail(fmt.Errorf("devsup: unknown op %q", req.Op))
}

func (s *server) start(spec hostsvc.UnitSpec) (hostsvc.UnitStatus, error) {
	if err := hostsvc.ValidateSpec(spec); err != nil {
		return hostsvc.UnitStatus{}, err
	}
	if err := hostsvc.RefuseUnsupported(hostsvc.ProcCapabilities(), spec); err != nil {
		return hostsvc.UnitStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return hostsvc.UnitStatus{}, errStopping
	default:
	}
	if u, ok := s.units[spec.Name]; ok {
		st := u.st
		st.Adopted = true
		return st, nil
	}
	for _, w := range spec.Wants {
		if err := hostsvc.ValidateName(w); err != nil {
			return hostsvc.UnitStatus{}, err
		}
		if _, ok := s.units[w]; !ok {
			return hostsvc.UnitStatus{}, fmt.Errorf("devsup: %s wants %s, which this devsup does not hold", spec.Name, w)
		}
	}
	for _, a := range spec.After {
		if err := hostsvc.ValidateName(a); err != nil {
			return hostsvc.UnitStatus{}, err
		}
	}
	s.seq++
	u := &unit{spec: spec, order: s.seq}
	u.st = hostsvc.UnitStatus{Name: spec.Name, Backend: hostsvc.BackendProc, Description: spec.Description}
	s.units[spec.Name] = u
	if err := s.openFDStore(u); err != nil {
		delete(s.units, spec.Name)
		return u.st, err
	}
	if err := s.launch(u); err != nil {
		return u.st, err
	}
	return u.st, nil
}

func (s *server) openOutput(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

func (s *server) unitEnv(u *unit, inv string) []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if s.o.Instance != "" {
		env = append(env, EnvInstance+"="+s.o.Instance)
	}
	env = append(env, u.spec.Env...)
	env = append(env, hostsvc.ReadEnvFiles(u.spec.EnvFiles)...)
	return append(env, EnvUnit+"="+u.spec.Name, EnvDevsup+"="+s.id, EnvInvocation+"="+inv)
}

func (s *server) launch(u *unit) error {
	logPath := filepath.Join(s.o.State, "logs", u.spec.Name+".log")
	outPath, errPath := u.spec.Output.StdoutFile, u.spec.Output.StderrFile
	outPath = cmp.Or(outPath, logPath)
	errPath = cmp.Or(errPath, logPath)
	out, err := s.openOutput(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	errf, err := s.openOutput(errPath)
	if err != nil {
		return err
	}
	defer errf.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer null.Close()
	dir := u.spec.WorkDir
	dir = cmp.Or(dir, "/")
	inv := newID()
	env := s.unitEnv(u, inv)
	files := []*os.File{null, out, errf}
	if u.store != nil {
		env = append(env, EnvNotifySocket+"="+u.store.path)
		stored, listenEnv := u.store.launchFiles()
		files = append(files, stored...)
		env = append(env, listenEnv...)
	}
	attr := &os.ProcAttr{Dir: dir, Env: env, Files: files,
		Sys: &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}}
	reply := make(chan spawnResult, 1)
	var r spawnResult
	select {
	case s.spawn <- spawnReq{argv: u.spec.Exec, attr: attr, reply: reply}:
		r = <-reply
	case <-s.done:
		r = spawnResult{err: errStopping}
	}
	u.stopping, u.timedOut = false, false
	u.st.InvocationID = inv
	u.st.ActiveSince = time.Now().UTC()
	if r.err != nil {
		u.st.State, u.st.SubState, u.st.Result = hostsvc.Failed, "failed", "exit-code"
		u.st.ExecMainCode, u.st.ExecMainStatus, u.st.MainPID = CodeExited, ExitStatusExec, 0
		s.persist(u)
		s.emit(u)
		return fmt.Errorf("devsup: the start job of %s ended failed: exec %s: %w", u.spec.Name, u.spec.Exec[0], r.err)
	}
	u.pgid = r.pid
	u.leaderTicks, _ = hostos.ProcStart(r.pid)
	u.exited = make(chan struct{})
	u.st.MainPID = r.pid
	u.st.State, u.st.SubState, u.st.Result = hostsvc.Active, "running", "success"
	u.st.ExecMainCode, u.st.ExecMainStatus = 0, 0
	s.persist(u)
	s.emit(u)
	s.log.Info("unit started", "unit", u.spec.Name, "pid", r.pid, "invocation", inv, "exec", u.spec.Exec)
	return nil
}

func (s *server) inspect(name string) hostsvc.UnitStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.units[name]; ok {
		return u.st
	}
	return hostsvc.UnitStatus{Name: name, State: hostsvc.NotFound, Backend: hostsvc.BackendProc}
}

func (s *server) stop(name string) error {
	s.mu.Lock()
	u, ok := s.units[name]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	if u.restartT != nil {
		u.restartT.Stop()
	}
	if u.exited == nil {
		u.stopping = true
		if u.st.SubState == "auto-restart" {
			u.st.State, u.st.SubState = hostsvc.Failed, "failed"
			s.persist(u)
			s.emit(u)
		}
		s.mu.Unlock()
		return nil
	}
	u.stopping = true
	u.st.State, u.st.SubState = hostsvc.Deactivating, "stop-sigterm"
	s.emit(u)
	exited, pgid, inv, sig, grace := u.exited, u.pgid, u.st.InvocationID, u.killSignal(), u.stopTimeout()
	s.mu.Unlock()
	set := s.members(name, inv, pgid)
	for _, pid := range set {
		syscall.Kill(pid, sig)
	}
	deadline := time.Now().Add(grace)
	select {
	case <-exited:
	case <-s.done:
	case <-time.After(grace):
		s.mu.Lock()
		u.timedOut = true
		s.mu.Unlock()
		for _, pid := range s.members(name, inv, pgid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-exited:
		case <-s.done:
		}
	}
	s.awaitGone(name, inv, pgid, deadline)
	for _, pid := range s.members(name, inv, pgid) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	s.log.Info("unit stopped", "unit", name, "signal", sig.String(), "took", time.Since(deadline.Add(-grace)).Round(time.Millisecond).String())
	return nil
}

func (s *server) stopSlice(slice string) error {
	if err := hostsvc.ValidateSlice(slice); err != nil {
		return err
	}
	s.mu.Lock()
	var hit []*unit
	for _, u := range s.units {
		if hostsvc.SliceContains(slice, u.spec.Slice) {
			hit = append(hit, u)
		}
	}
	s.mu.Unlock()
	sort.Slice(hit, func(i, j int) bool { return hit[i].order > hit[j].order })
	var errs []error
	for _, u := range hit {
		if err := s.stop(u.spec.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *server) resetFailed(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.units[name]
	if !ok || u.st.State != hostsvc.Failed {
		return
	}
	s.collect(u)
}

func (s *server) list(prefix string) []hostsvc.UnitStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []hostsvc.UnitStatus
	for name, u := range s.units {
		if strings.HasPrefix(name, prefix) {
			out = append(out, u.st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *server) belongs(name, inv string, pgid, pid int) bool {
	if p, ok := readProcInfo(pid); ok && pgid > 0 && p.pgid == pgid {
		return true
	}
	env := procEnv(pid, EnvDevsup, EnvUnit, EnvInvocation)
	return env[EnvDevsup] == s.id && env[EnvUnit] == name && env[EnvInvocation] == inv
}

func (s *server) identify(name string, pid int) (hostsvc.Identity, error) {
	s.mu.Lock()
	u, ok := s.units[name]
	var st hostsvc.UnitStatus
	var pgid int
	if ok {
		st, pgid = u.st, u.pgid
	}
	s.mu.Unlock()
	if !ok || st.State != hostsvc.Active {
		return hostsvc.Identity{}, fmt.Errorf("hostsvc: %s is %s, not active, and has no identity to record", name, s.inspect(name).State)
	}
	pid = cmp.Or(pid, st.MainPID)
	ticks, err := pin(pid)
	if err != nil {
		return hostsvc.Identity{}, fmt.Errorf("hostsvc: pinning pid %d of %s: %w", pid, name, err)
	}
	if !s.belongs(name, st.InvocationID, pgid, pid) {
		return hostsvc.Identity{}, fmt.Errorf("hostsvc: pid %d is neither in the process group of %s nor attributed to its invocation %s",
			pid, name, st.InvocationID)
	}
	return hostsvc.Identity{Unit: name, InvocationID: st.InvocationID, PID: pid, StartTicks: ticks, BootID: s.boot,
		Backend: hostsvc.BackendProc, Manager: s.id}, nil
}

var errGone = errors.New("the process is gone")

func pin(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("%w: pid %d", errGone, pid)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return 0, fmt.Errorf("%w: pidfd_open(%d): %v", errGone, pid, err)
	}
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	p, ok := readProcInfo(pid)
	if !ok || p.state == 'Z' || p.state == 'X' {
		return 0, fmt.Errorf("%w: pid %d", errGone, pid)
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return 0, fmt.Errorf("%w: pidfd_send_signal(%d, 0): %v", errGone, pid, err)
	}
	return p.ticks, nil
}

func (s *server) check(want hostsvc.Identity) (hostsvc.Observation, error) {
	if want.BootID != s.boot {
		return hostsvc.Observation{Verdict: hostsvc.VerdictLost, Reason: hostsvc.LostHostRebooted,
			Detail: fmt.Sprintf("boot id %s, recorded %s", s.boot, want.BootID)}, nil
	}
	if obs, foreign := hostsvc.ForeignIdentity(hostsvc.ProcCapabilities(), want); foreign {
		return obs, nil
	}
	st := s.inspect(want.Unit)
	lost := func(r hostsvc.LostReason, format string, args ...any) (hostsvc.Observation, error) {
		return hostsvc.Observation{Verdict: hostsvc.VerdictLost, Reason: r, Detail: fmt.Sprintf(format, args...), Status: st}, nil
	}
	if want.Manager != "" && want.Manager != s.id {
		return lost(hostsvc.LostUnitGone, "devsup restarted (%s, recorded %s): every unit it held is gone", s.id, want.Manager)
	}
	switch st.State {
	case hostsvc.NotFound:
		return lost(hostsvc.LostUnitGone, "%s is not loaded", want.Unit)
	case hostsvc.Failed:
		return lost(hostsvc.LostUnitFailed, "%s failed: result %s, exec main code %d status %d",
			want.Unit, st.Result, st.ExecMainCode, st.ExecMainStatus)
	case hostsvc.Inactive:
		return lost(hostsvc.LostUnitExited, "%s is inactive: result %s", want.Unit, st.Result)
	}
	if st.InvocationID != want.InvocationID {
		return s.mismatch(st, "%s runs invocation %s, recorded %s", want.Unit, st.InvocationID, want.InvocationID)
	}
	ticks, err := pin(want.PID)
	if errors.Is(err, errGone) {
		return s.mismatch(st, "recorded pid %d of %s: %v", want.PID, want.Unit, err)
	}
	if err != nil {
		return hostsvc.Observation{Detail: err.Error(), Status: st}, err
	}
	if ticks != want.StartTicks {
		return s.mismatch(st, "pid %d started at tick %d, recorded %d: the pid was reused", want.PID, ticks, want.StartTicks)
	}
	s.mu.Lock()
	pgid := 0
	if u, ok := s.units[want.Unit]; ok {
		pgid = u.pgid
	}
	s.mu.Unlock()
	if !s.belongs(want.Unit, st.InvocationID, pgid, want.PID) {
		return s.mismatch(st, "pid %d is not in %s invocation %s", want.PID, want.Unit, st.InvocationID)
	}
	if st.State != hostsvc.Active {
		return hostsvc.Observation{Verdict: hostsvc.VerdictUnknown, Detail: fmt.Sprintf("%s is %s", want.Unit, st.State), Status: st}, nil
	}
	return hostsvc.Observation{Verdict: hostsvc.VerdictRunning, Status: st}, nil
}

func (s *server) mismatch(st hostsvc.UnitStatus, format string, args ...any) (hostsvc.Observation, error) {
	obs := hostsvc.Observation{Verdict: hostsvc.VerdictLost, Reason: hostsvc.LostIdentityMismatch,
		Detail: fmt.Sprintf(format, args...), Status: st}
	if err := s.stop(st.Name); err != nil {
		return obs, fmt.Errorf("hostsvc: %s does not match its record and stopping it failed: %w", st.Name, err)
	}
	obs.Stopped = true
	return obs, nil
}

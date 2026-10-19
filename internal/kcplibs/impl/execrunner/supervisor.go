package execrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type runState struct {
	PID int `json:"pid"`

	Started string `json:"started"`

	Ticks uint64 `json:"ticks"`

	Stopped bool `json:"stopped"`

	Assets string `json:"assets,omitempty"`
}

type process struct {
	dir string

	cmd *exec.Cmd

	done chan struct{}

	waitErr error

	pid int

	ticks uint64

	started time.Time

	stopped atomic.Bool

	exitCode int32

	env map[string]string

	assets string

	memoryLimit uint64

	oomKilled bool
}

type processSpec struct {
	binary string

	args []string

	dir string

	files map[string][]byte

	env []string

	envMap map[string]string

	envForDir func(dir string, caller []string) []string

	memoryMax uint64

	memoryLimit uint64
}

const retainedRunsLimit = 128

type supervisor struct {
	prefix string

	runsDir string

	doneFile string

	mu sync.Mutex

	runs map[string]*process

	envs map[string]map[string]string

	completed []string

	retainLimit int

	seq atomic.Int64

	holder Holder
}

type Holder func() (int, error)

func newSupervisor(prefix, runsDir, doneFile string, holder Holder) *supervisor {
	return &supervisor{prefix: prefix, runsDir: runsDir, doneFile: doneFile, holder: holder, runs: map[string]*process{}, envs: map[string]map[string]string{}, retainLimit: retainedRunsLimit}
}

func (s *supervisor) nextID() string {
	return fmt.Sprintf("%s-%s-%d", s.prefix, time.Now().UTC().Format("20060102T150405.000000000"), s.seq.Add(1))
}

func (s *supervisor) start(entry processSpec) (string, error) {
	id := s.nextID()
	dir := filepath.Join(s.runsDir, id)
	if err := os.MkdirAll(s.runsDir, 0o755); err != nil {
		return "", fmt.Errorf("execrunner: create the runs directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", fmt.Errorf("execrunner: create the %s directory: %w", s.prefix, err)
	}
	abandon := func(err error) (string, error) {
		_ = os.RemoveAll(dir)
		return "", err
	}

	for name, body := range entry.files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return abandon(fmt.Errorf("execrunner: write %s: %w", name, err))
		}
	}
	stdout, err := openLog(filepath.Join(dir, StdoutFile))
	if err != nil {
		return abandon(fmt.Errorf("execrunner: create stdout file: %w", err))
	}
	defer stdout.Close()
	stderr, err := openLog(filepath.Join(dir, StderrFile))
	if err != nil {
		return abandon(fmt.Errorf("execrunner: create stderr file: %w", err))
	}
	defer stderr.Close()

	env := append([]string{}, entry.env...)
	if entry.envForDir != nil {
		env = append(env, entry.envForDir(dir, entry.env)...)
	}

	cmd := exec.Command(entry.binary, entry.args...)
	cmd.Dir = entry.dir
	if entry.dir == "" {
		cmd.Dir = dir
	}
	cmd.Env = append(detached(os.Environ()), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := host.prepare(cmd); err != nil {
		return abandon(err)
	}
	var lim *limiter
	if entry.memoryMax > 0 || s.holder != nil {
		if lim, err = newLimiter(cmd, id, entry.memoryMax, s.holder); err != nil {
			return abandon(err)
		}
	}
	if err := cmd.Start(); err != nil {
		lim.release()
		return abandon(fmt.Errorf("execrunner: start %s process: %w", s.prefix, err))
	}
	lim.started()

	run := &process{
		dir:     dir,
		cmd:     cmd,
		done:    make(chan struct{}),
		pid:     cmd.Process.Pid,
		ticks:   startTicks(cmd.Process.Pid),
		started: time.Now(),
		env:     entry.envMap,
		assets:  envValue(entry.env, "SOCIALWEB_TS_KEY"),

		memoryLimit: entry.memoryLimit,
	}
	if err := writeState(run); err != nil {
		host.killGroup(run.pid)
		lim.release()
		return abandon(err)
	}
	s.mu.Lock()
	s.runs[id] = run
	s.envs[id] = run.env
	s.mu.Unlock()

	go func() {
		run.waitErr = cmd.Wait()
		if cmd.ProcessState != nil {
			run.exitCode = int32(cmd.ProcessState.ExitCode())
		}
		run.oomKilled = lim.oomKilled()
		lim.release()
		writeDone(run, s.doneFile)
		close(run.done)
		if s.doneFile != "" {
			s.forget(id)
		}
		s.retain(id)
	}()
	return id, nil
}

func (s *supervisor) held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *supervisor) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, id)
}

func (s *supervisor) retain(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, id)
	for len(s.completed) > s.retainLimit {
		oldest := s.completed[0]
		s.completed = s.completed[1:]
		delete(s.runs, oldest)
		delete(s.envs, oldest)
	}
}

func (s *supervisor) remove(id string) error {
	if !IsRunID(id) || !strings.HasPrefix(id, s.prefix+"-") {
		return nil
	}
	_ = s.stop(id)
	s.mu.Lock()
	delete(s.runs, id)
	delete(s.envs, id)
	s.mu.Unlock()
	removePodCgroup(id)
	return os.RemoveAll(filepath.Join(s.runsDir, id))
}

func openLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func (s *supervisor) retained() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs)
}

func writeDone(run *process, doneFile string) {
	if doneFile == "" {
		return
	}
	body, err := json.Marshal(podDone{ExitCode: run.exitCode, OOMKilled: run.oomKilled, MemoryLimit: run.memoryLimit})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(run.dir, doneFile), body, 0o644)
}

func (s *supervisor) recordAdoptedOOMKill(run *process) {
	if run.cmd != nil || s.doneFile == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(run.dir, s.doneFile)); err == nil {
		return
	}
	limit := oomKilledLimit(filepath.Base(run.dir), s.holder)
	if limit == 0 {
		return
	}
	run.exitCode, run.oomKilled, run.memoryLimit = 137, true, limit
	writeDone(run, s.doneFile)
}

func writeState(run *process) error {
	body, err := json.Marshal(runState{PID: run.pid, Started: run.started.Format(time.RFC3339Nano), Ticks: run.ticks, Stopped: run.stopped.Load(), Assets: run.assets})
	if err != nil {
		return fmt.Errorf("execrunner: encode the run state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(run.dir, "state.json"), body, 0o644); err != nil {
		return fmt.Errorf("execrunner: write the run state: %w", err)
	}
	return nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func (s *supervisor) lookup(id string) (*process, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	return run, ok
}

func (s *supervisor) dir(id string) (string, error) {
	if run, ok := s.lookup(id); ok {
		return run.dir, nil
	}
	recovered, err := s.recover(id)
	if err != nil {
		return "", err
	}
	return recovered.dir, nil
}

func (s *supervisor) recover(id string) (*process, error) {
	dir := filepath.Join(s.runsDir, id)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("execrunner: no %s run directory for %s", s.prefix, id)
	}
	var state runState
	if body, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		_ = json.Unmarshal(body, &state)
	}
	started := time.Now()
	if state.Started != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, state.Started); err == nil {
			started = parsed
		}
	}
	recovered := &process{dir: dir, pid: state.PID, ticks: state.Ticks, started: started, assets: state.Assets}
	recovered.stopped.Store(state.Stopped)
	return recovered, nil
}

func (s *supervisor) envOf(id string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run, ok := s.runs[id]; ok {
		return run.env
	}
	if env, ok := s.envs[id]; ok {
		return env
	}
	if run, err := s.recover(id); err == nil && ownedByUs(run.pid, run.ticks) {
		return host.environ(run.pid)
	}
	return nil
}

func (s *supervisor) resolve(id string) (*process, error) {
	if run, ok := s.lookup(id); ok {
		return run, nil
	}
	return s.recover(id)
}

func (s *supervisor) finished(run *process) bool {
	if run.cmd != nil {
		select {
		case <-run.done:
			return true
		default:
			return false
		}
	}
	if run.stopped.Load() {
		return true
	}
	if s.doneFile != "" {
		if _, err := os.Stat(filepath.Join(run.dir, s.doneFile)); err == nil {
			return true
		}
	}
	if !processAlive(run.pid) {
		return true
	}
	if run.ticks == 0 {
		return false
	}
	now := startTicks(run.pid)
	return now != 0 && now != run.ticks
}

const stopGrace = 2 * time.Second

func (s *supervisor) stop(id string) error {
	run, err := s.resolve(id)
	if err != nil {
		return nil
	}
	if s.finished(run) {
		return nil
	}
	if run.cmd == nil && run.ticks != 0 && !ownedByUs(run.pid, run.ticks) {
		return nil
	}
	run.stopped.Store(true)
	_ = writeState(run)
	host.killGroup(run.pid)
	if run.cmd != nil {
		select {
		case <-run.done:
		case <-time.After(stopGrace):
		}
	}
	return nil
}

func (s *supervisor) probe(ctx context.Context, id string, command []string, timeout time.Duration, withEnv bool) (bool, error) {
	if len(command) == 0 {
		return true, nil
	}
	dir, err := s.dir(id)
	if err != nil {
		return false, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, command[0], command[1:]...)
	cmd.Dir = dir
	if withEnv {
		cmd.Env = append(os.Environ(), envPairs(s.envOf(id))...)
	}
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}

func env(req map[string]string, extra []string) []string {
	out := append([]string{}, extra...)
	keys := make([]string, 0, len(req))
	for key := range req {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+req[key])
	}
	return out
}

func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

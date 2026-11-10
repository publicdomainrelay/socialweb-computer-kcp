package guestagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	execTimeoutDefault = 60 * time.Second

	execTimeoutMax = 600 * time.Second

	execOutputCap = 256 << 10

	jobOutputCap = 16 << 20

	readMaxDefault = 64 << 10

	readMaxCap = 256 << 10

	readWaitMax = 30 * time.Second

	jobStdinWriteTimeout = 30 * time.Second

	jobWaitDelay = 5 * time.Second

	maxJobs = 128
)

type jobTable struct {
	mu sync.Mutex

	jobs map[string]*job
}

type job struct {
	id string

	command string

	startedAt time.Time

	cmd *exec.Cmd

	stdinMu sync.Mutex

	stdin *os.File

	mu sync.Mutex

	buf []byte

	base int64

	exited bool

	exitCode int

	changed chan struct{}
}

type execResult struct {
	ExitCode int `json:"exitCode"`

	Stdout string `json:"stdout"`

	Stderr string `json:"stderr"`

	Truncated bool `json:"truncated"`

	TimedOut bool `json:"timedOut,omitempty"`
}

type readResult struct {
	Output string `json:"output"`

	Cursor int64 `json:"cursor"`

	Done bool `json:"done"`

	ExitCode *int `json:"exitCode,omitempty"`

	Dropped int64 `json:"dropped,omitempty"`
}

type jobInfo struct {
	ID string `json:"id"`

	Command string `json:"command"`

	StartedAt string `json:"startedAt"`

	Done bool `json:"done"`

	ExitCode *int `json:"exitCode,omitempty"`
}

func newJobTable() *jobTable { return &jobTable{jobs: map[string]*job{}} }

func shell(ctx context.Context, command, cwd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = os.Environ()
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	cmd.Dir = cwd
	if cmd.Dir == "" {
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			cmd.Dir = home
		}
	}
	setGroup(cmd)
	return cmd
}

type cappedBuffer struct {
	buf bytes.Buffer

	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := execOutputCap - c.buf.Len()
	if len(p) > room {
		c.truncated = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func runExec(ctx context.Context, command, stdin, cwd string, timeout time.Duration) (execResult, error) {
	if timeout <= 0 {
		timeout = execTimeoutDefault
	}
	timeout = min(timeout, execTimeoutMax)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := shell(ctx, command, cwd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Cancel = func() error { return signalGroup(cmd, "KILL") }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := execResult{Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), Truncated: stdout.truncated || stderr.truncated}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.ExitCode, res.TimedOut = -1, true
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	case err != nil && cmd.ProcessState == nil:
		return res, err
	case cmd.ProcessState != nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	return res, nil
}

func (t *jobTable) start(command, cwd, stdin string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.jobs) >= maxJobs && !t.evictLocked() {
		return "", fmt.Errorf("%d jobs are running; kill one first", len(t.jobs))
	}
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	j := &job{id: newID(), command: command, startedAt: time.Now().UTC(), stdin: w, changed: make(chan struct{})}
	cmd := shell(context.Background(), command, cwd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r, j, j
	cmd.WaitDelay = jobWaitDelay
	j.cmd = cmd
	err = cmd.Start()
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return "", err
	}
	t.jobs[j.id] = j
	go j.wait()
	if stdin != "" {
		go func() { _ = j.write(stdin, false) }()
	}
	return j.id, nil
}

func (t *jobTable) evictLocked() bool {
	var oldest *job
	for _, j := range t.jobs {
		j.mu.Lock()
		exited := j.exited
		j.mu.Unlock()
		if exited && (oldest == nil || j.startedAt.Before(oldest.startedAt)) {
			oldest = j
		}
	}
	if oldest == nil {
		return false
	}
	delete(t.jobs, oldest.id)
	return true
}

func (t *jobTable) get(id string) (*job, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return nil, fmt.Errorf("no job %q", id)
	}
	return j, nil
}

func (t *jobTable) list() []jobInfo {
	t.mu.Lock()
	jobs := make([]*job, 0, len(t.jobs))
	for _, j := range t.jobs {
		jobs = append(jobs, j)
	}
	t.mu.Unlock()
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].startedAt.Before(jobs[b].startedAt) })
	out := make([]jobInfo, 0, len(jobs))
	for _, j := range jobs {
		j.mu.Lock()
		info := jobInfo{ID: j.id, Command: j.command, StartedAt: j.startedAt.Format(time.RFC3339Nano), Done: j.exited}
		if j.exited {
			code := j.exitCode
			info.ExitCode = &code
		}
		j.mu.Unlock()
		out = append(out, info)
	}
	return out
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	j.buf = append(j.buf, p...)
	if over := len(j.buf) - jobOutputCap; over > 0 {
		j.buf = j.buf[over:]
		j.base += int64(over)
	}
	j.notifyLocked()
	j.mu.Unlock()
	return len(p), nil
}

func (j *job) notifyLocked() {
	close(j.changed)
	j.changed = make(chan struct{})
}

func (j *job) wait() {
	_ = j.cmd.Wait()
	j.stdinMu.Lock()
	_ = j.stdin.Close()
	j.stdinMu.Unlock()
	j.mu.Lock()
	j.exited = true
	j.exitCode = j.cmd.ProcessState.ExitCode()
	j.notifyLocked()
	j.mu.Unlock()
}

func (j *job) read(ctx context.Context, cursor int64, maxBytes int, wait time.Duration) readResult {
	if maxBytes <= 0 {
		maxBytes = readMaxDefault
	}
	maxBytes = min(maxBytes, readMaxCap)
	deadline := time.NewTimer(min(max(wait, 0), readWaitMax))
	defer deadline.Stop()
	for {
		j.mu.Lock()
		end := j.base + int64(len(j.buf))
		if cursor >= end && !j.exited && wait > 0 {
			changed := j.changed
			j.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-deadline.C:
				wait = 0
				continue
			case <-ctx.Done():
				wait = 0
				continue
			}
		}
		res := readResult{}
		cursor = min(max(cursor, 0), end)
		if cursor < j.base {
			res.Dropped = j.base - cursor
			cursor = j.base
		}
		chunk := j.buf[cursor-j.base : min(int64(len(j.buf)), cursor-j.base+int64(maxBytes))]
		if trimmed := trimPartialRune(chunk); len(trimmed) > 0 {
			chunk = trimmed
		}
		res.Output = string(chunk)
		res.Cursor = cursor + int64(len(chunk))
		res.Done = j.exited && res.Cursor == end
		if j.exited {
			code := j.exitCode
			res.ExitCode = &code
		}
		j.mu.Unlock()
		return res
	}
}

func trimPartialRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if !utf8.RuneStart(b[len(b)-i]) {
			continue
		}
		if !utf8.FullRune(b[len(b)-i:]) {
			return b[:len(b)-i]
		}
		return b
	}
	return b
}

func (j *job) write(data string, closeStdin bool) error {
	j.stdinMu.Lock()
	defer j.stdinMu.Unlock()
	j.mu.Lock()
	exited := j.exited
	j.mu.Unlock()
	if exited {
		return errors.New("the job is done")
	}
	if data != "" {
		_ = j.stdin.SetWriteDeadline(time.Now().Add(jobStdinWriteTimeout))
		if _, err := j.stdin.WriteString(data); err != nil {
			return fmt.Errorf("writing the job's stdin: %w", err)
		}
	}
	if closeStdin {
		if err := j.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			return err
		}
	}
	return nil
}

func (j *job) kill(signal string) error {
	j.mu.Lock()
	exited := j.exited
	j.mu.Unlock()
	if exited {
		return nil
	}
	return signalGroup(j.cmd, signal)
}

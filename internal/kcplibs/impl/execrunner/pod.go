package execrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/runner"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/listeners"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/outputs"
)

const DefaultPodTimeout = 5 * time.Minute

const DefaultResultFile = "result.json"

const PortFileName = "port-bound-to"

const PodFile = "service.txt"

type PodOptions struct {
	DenoBin string

	RunsDir string

	NamespaceDir string

	ExtraEnv []string

	Timeout time.Duration

	ResultFile string

	CAData []byte

	TrustBundle func() []byte

	Unit string

	EnforceMemory bool
}

type Pod struct {
	opts PodOptions

	sup *supervisor
}

var _ runner.PodRunner = (*Pod)(nil)

type podDone struct {
	ExitCode int32 `json:"exitCode"`

	OOMKilled bool `json:"oomKilled,omitempty"`

	MemoryLimit uint64 `json:"memoryLimit,omitempty"`
}

func NewPod(opts PodOptions) (*Pod, error) {
	if opts.DenoBin == "" {
		opts.DenoBin = "deno"
	}
	if opts.RunsDir == "" {
		return nil, errors.New("execrunner: RunsDir is required")
	}
	if opts.ResultFile == "" {
		opts.ResultFile = DefaultResultFile
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultPodTimeout
	}
	abs, err := filepath.Abs(opts.RunsDir)
	if err != nil {
		return nil, fmt.Errorf("execrunner: resolve RunsDir: %w", err)
	}
	opts.RunsDir = abs
	return &Pod{opts: opts, sup: newSupervisor("pod", abs, "done.json", opts.Unit)}, nil
}

func (p *Pod) Start(_ context.Context, req runner.PodRequest) (string, error) {
	files := map[string][]byte{"main.ts": []byte(req.Script), PodFile: []byte(req.Service + "\n")}
	if req.DenoJSON != "" {
		files["deno.json"] = []byte(req.DenoJSON)
	}
	if req.DenoLock != "" {
		files["deno.lock"] = []byte(req.DenoLock)
	}
	if bundle := p.trustBundle(); len(bundle) > 0 {
		files["ca.pem"] = bundle
	}
	args := []string{"run"}
	env := p.env(req)
	if req.MemoryMax > 0 {
		heap := "--max-old-space-size=" + strconv.FormatUint(heapMiB(req.MemoryMax), 10)
		args = append(args, "--v8-flags="+heap)
		env = append(env, "DENO_V8_FLAGS="+heap)
	}
	args = append(args, req.PermissionArgs...)
	args = append(args, "main.ts")
	return p.sup.start(processSpec{
		binary:    p.opts.DenoBin,
		args:      args,
		files:     files,
		env:       env,
		envMap:    req.Env,
		envForDir: p.dirEnv,
		memoryMax: p.enforced(req.MemoryMax),

		memoryLimit: req.MemoryMax,
	})
}

func (p *Pod) Observe(_ context.Context, runID string) (runner.PodStatus, error) {
	run, err := p.sup.resolve(runID)
	if err != nil {
		return runner.PodStatus{}, fmt.Errorf("execrunner: unknown pod run %s", runID)
	}
	if !p.sup.finished(run) {
		timeout := p.opts.Timeout
		if timeout > 0 && time.Since(run.started) > timeout {
			_ = p.sup.stop(runID)
			return runner.PodStatus{State: runner.StateFailed, Message: "the deno process exceeded the runner timeout"}, nil
		}
		return runner.PodStatus{State: runner.StateRunning, BoundPort: boundPort(run.dir), Listeners: reportedListeners(run.dir)}, nil
	}
	if run.stopped.Load() {
		return runner.PodStatus{State: runner.StateFailed, Message: "the deno process was stopped"}, nil
	}
	status, err := p.result(run)
	if err == nil {
		p.classify(run, &status)
	}
	return status, err
}

func (p *Pod) enforced(memoryMax uint64) uint64 {
	if p.opts.EnforceMemory {
		return memoryMax
	}
	return 0
}

func heapMiB(memoryMax uint64) uint64 {
	return memoryMax / 100 * 80 >> 20
}

func (p *Pod) classify(run *process, status *runner.PodStatus) {
	if status.State != runner.StateFailed {
		return
	}
	var done podDone
	body, err := os.ReadFile(filepath.Join(run.dir, p.sup.doneFile))
	if err != nil || json.Unmarshal(body, &done) != nil || done.MemoryLimit == 0 {
		return
	}
	if done.OOMKilled {
		status.Reason = runner.ReasonOOMKilled
		status.Message = fmt.Sprintf("the deno process was killed for exceeding its memory limit of %d bytes", done.MemoryLimit)
		return
	}
	if strings.Contains(stderrTail(run.dir), "out of memory") {
		status.Reason = runner.ReasonHeapLimitExceeded
		status.Message = fmt.Sprintf("the deno process exceeded its V8 heap limit of %d MiB (80%% of its memory limit)", heapMiB(done.MemoryLimit))
	}
}

func stderrTail(dir string) string {
	f, err := os.Open(filepath.Join(dir, StderrFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	const tail = 64 << 10
	buf := make([]byte, min(info.Size(), tail))
	n, _ := f.ReadAt(buf, info.Size()-int64(len(buf)))
	return string(buf[:n])
}

func (p *Pod) Stop(_ context.Context, runID string) error {
	return p.sup.stop(runID)
}

func (p *Pod) Probe(ctx context.Context, runID string, command []string, timeout time.Duration) (bool, error) {
	return p.sup.probe(ctx, runID, command, timeout, true)
}

func (p *Pod) result(run *process) (runner.PodStatus, error) {
	exit, err := p.exitCode(run)
	if err != nil {
		found, oerr := p.readOutputs(run)
		if oerr == nil && len(found) > 0 {
			return runner.PodStatus{State: runner.StateSucceeded, Outputs: found}, nil
		}
		return runner.PodStatus{State: runner.StateFailed, Message: err.Error()}, nil
	}
	status := runner.PodStatus{ExitCode: exit}
	if run.waitErr != nil {
		status.Message = fmt.Sprintf("the deno process failed: %v", run.waitErr)
	}
	if exit == 0 {
		status.State = runner.StateSucceeded
	} else {
		status.State = runner.StateFailed
		if status.Message == "" {
			status.Message = fmt.Sprintf("the deno process exited with code %d", exit)
		}
	}
	found, err := p.readOutputs(run)
	if err != nil {
		return runner.PodStatus{State: runner.StateFailed, ExitCode: exit, Message: err.Error()}, nil
	}
	status.Outputs = found
	return status, nil
}

func (p *Pod) exitCode(run *process) (int32, error) {
	body, err := os.ReadFile(filepath.Join(run.dir, p.sup.doneFile))
	if err == nil {
		var done podDone
		if err := json.Unmarshal(body, &done); err == nil {
			return done.ExitCode, nil
		}
	}
	return 0, errors.New("the deno process did not report an exit code")
}

func (p *Pod) readOutputs(run *process) (map[string]string, error) {
	body, err := os.ReadFile(filepath.Join(run.dir, p.opts.ResultFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading pod outputs: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing pod outputs: %w", err)
	}
	return outputs.Stringify(raw), nil
}

func (p *Pod) trustBundle() []byte {
	if p.opts.TrustBundle != nil {
		return p.opts.TrustBundle()
	}
	return p.opts.CAData
}

func (p *Pod) env(req runner.PodRequest) []string {
	env := env(req.Env, p.opts.ExtraEnv)
	if req.Token != "" {
		env = append(env, "KCP_TOKEN="+req.Token)
	}
	if req.Server != "" {
		env = append(env, "KCP_SERVER="+req.Server)
	}
	if req.Workspace != "" {
		env = append(env, "KCP_WORKSPACE="+req.Workspace)
	}
	return env
}

func (p *Pod) dirEnv(dir string, caller []string) []string {
	var env []string
	if !containsKey(caller, "DENO_DIR") {
		moduleDir := p.opts.NamespaceDir
		if moduleDir == "" {
			moduleDir = filepath.Join(dir, ".deno")
		}
		env = append(env, "DENO_DIR="+moduleDir)
	}
	if !containsKey(caller, "PORT_FILE") {
		env = append(env, "PORT_FILE="+filepath.Join(dir, PortFileName))
	}
	if !containsKey(caller, listeners.FileEnv) {
		env = append(env, listeners.FileEnv+"="+filepath.Join(dir, listeners.FileName))
	}
	if len(p.trustBundle()) > 0 && !containsKey(caller, "DENO_CERT") {
		env = append(env, "DENO_CERT="+filepath.Join(dir, "ca.pem"))
	}
	return env
}

func boundPort(dir string) int32 {
	body, err := os.ReadFile(filepath.Join(dir, PortFileName))
	if err != nil {
		return 0
	}
	port, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 16)
	if err != nil {
		return 0
	}
	return int32(port)
}

func reportedListeners(dir string) []listeners.Listener {
	body, err := os.ReadFile(filepath.Join(dir, listeners.FileName))
	if err != nil {
		return nil
	}
	found, err := listeners.Parse(body)
	if err != nil {
		return nil
	}
	return found
}

func containsKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

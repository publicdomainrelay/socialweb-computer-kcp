package memoryrunner

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/runner"
)

type PodOptions struct {
	Outcome runner.PodStatus

	PollsBeforeDone int

	ProbeResult bool
}

type Pod struct {
	opts PodOptions

	mu sync.Mutex

	runs map[string]*podRun

	seq atomic.Int64
}

var _ runner.PodRunner = (*Pod)(nil)

type podRun struct {
	req runner.PodRequest

	polls int

	status runner.PodStatus
}

func NewPod(opts PodOptions) *Pod {
	if opts.Outcome.State == "" {
		opts.Outcome = runner.PodStatus{State: runner.StateSucceeded}
	}
	opts.PollsBeforeDone = cmp.Or(opts.PollsBeforeDone, 1)
	return &Pod{opts: opts, runs: map[string]*podRun{}}
}

func (p *Pod) Start(_ context.Context, req runner.PodRequest) (string, error) {
	id := fmt.Sprintf("mempod-%d", p.seq.Add(1))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs[id] = &podRun{req: req, status: runner.PodStatus{State: runner.StateRunning}}
	return id, nil
}

func (p *Pod) Observe(_ context.Context, runID string) (runner.PodStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	run, ok := p.runs[runID]
	if !ok {
		return runner.PodStatus{}, fmt.Errorf("memoryrunner: unknown pod run %s", runID)
	}
	run.polls++
	if run.status.State == runner.StateRunning && run.polls >= p.opts.PollsBeforeDone {
		run.status = p.opts.Outcome
	}
	return run.status, nil
}

func (p *Pod) Stop(_ context.Context, runID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	run, ok := p.runs[runID]
	if !ok {
		return nil
	}
	if run.status.State == runner.StateRunning {
		run.status = runner.PodStatus{State: runner.StateFailed, Message: "stopped"}
	}
	return nil
}

func (p *Pod) Probe(_ context.Context, _ string, _ []string, _ time.Duration) (bool, error) {
	return p.opts.ProbeResult, nil
}

type EngineOptions struct {
	ExitsAfterPolls int
}

type Engine struct {
	opts EngineOptions

	mu sync.Mutex

	runs map[string]*engineRun

	seq atomic.Int64
}

var _ runner.EngineRunner = (*Engine)(nil)

type engineRun struct {
	polls int

	status runner.EngineStatus
}

func NewEngine(opts EngineOptions) *Engine {
	return &Engine{opts: opts, runs: map[string]*engineRun{}}
}

func (e *Engine) Start(_ context.Context, _ runner.EngineRequest) (string, error) {
	id := fmt.Sprintf("mengine-%d", e.seq.Add(1))
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs[id] = &engineRun{status: runner.EngineStatus{State: runner.StateRunning}}
	return id, nil
}

func (e *Engine) Observe(_ context.Context, runID string) (runner.EngineStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	run, ok := e.runs[runID]
	if !ok {
		return runner.EngineStatus{}, fmt.Errorf("memoryrunner: unknown engine run %s", runID)
	}
	run.polls++
	if run.status.State == runner.StateRunning && e.opts.ExitsAfterPolls > 0 && run.polls >= e.opts.ExitsAfterPolls {
		run.status = runner.EngineStatus{State: runner.StateFailed, Message: "exited"}
	}
	return run.status, nil
}

func (e *Engine) Stop(_ context.Context, runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if run, ok := e.runs[runID]; ok && run.status.State == runner.StateRunning {
		run.status = runner.EngineStatus{State: runner.StateFailed, Message: "stopped"}
	}
	return nil
}

func (e *Engine) Probe(_ context.Context, _ string, _ []string, _ time.Duration) (bool, error) {
	return true, nil
}

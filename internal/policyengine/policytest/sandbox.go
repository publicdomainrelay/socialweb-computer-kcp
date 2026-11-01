package policytest

import (
	"context"
	"maps"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const deadProxy = "http://127.0.0.1:9"

type SandboxMode int

const (
	SandboxFenced SandboxMode = iota
	SandboxUnfenced
	SandboxUnavailable
	SandboxHung
)

type Sandbox struct {
	inner    sandbox.Runner
	mode     atomic.Int32
	programs atomic.Int64
	actions  atomic.Int64

	hungMu   sync.Mutex
	hung     chan struct{}
	hungOnce *sync.Once
}

func (s *Sandbox) SetMode(mode SandboxMode) {
	s.hungMu.Lock()
	s.hung, s.hungOnce = make(chan struct{}), &sync.Once{}
	s.hungMu.Unlock()
	s.mode.Store(int32(mode))
}

func (s *Sandbox) HungStarted() <-chan struct{} {
	s.hungMu.Lock()
	defer s.hungMu.Unlock()
	if s.hung == nil {
		s.hung, s.hungOnce = make(chan struct{}), &sync.Once{}
	}
	return s.hung
}

func (s *Sandbox) Programs() int64 { return s.programs.Load() }

func (s *Sandbox) Actions() int64 { return s.actions.Load() }

func (s *Sandbox) Available() bool {
	return SandboxMode(s.mode.Load()) != SandboxUnavailable && s.inner.Available()
}

func (s *Sandbox) Fenced() bool {
	return SandboxMode(s.mode.Load()) != SandboxUnfenced && s.inner.Fenced()
}

func (s *Sandbox) Kind() string { return s.inner.Kind() }

func (s *Sandbox) Run(ctx context.Context, p sandbox.Program) (sandbox.Result, error) {
	s.programs.Add(1)
	if strings.Contains(p.Source, "__PE_RESULT__") {
		s.actions.Add(1)
	}
	if SandboxMode(s.mode.Load()) == SandboxHung {
		s.hungMu.Lock()
		hung, once := s.hung, s.hungOnce
		s.hungMu.Unlock()
		if once != nil {
			once.Do(func() { close(hung) })
		}
		<-ctx.Done()
		return sandbox.Result{TimedOut: true}, ctx.Err()
	}
	env := make(map[string]string, len(p.Env)+5)
	maps.Copy(env, p.Env)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		env[key] = deadProxy
	}
	env["NO_PROXY"] = ""
	p.Env = env
	return s.inner.Run(ctx, p)
}

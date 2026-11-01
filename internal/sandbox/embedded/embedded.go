package embedded

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const (
	Kind = policyv1alpha1.PolicySandboxProviderEmbedded

	DefaultTimeout = 2 * time.Minute

	OutputLimit = 64 << 10

	Grace = 5 * time.Second
)

var (
	_ sandbox.Runner = (*Runner)(nil)
	_ sandbox.Prober = (*Runner)(nil)
)

var ErrNoHost = errors.New("the app provided no policy sandbox (pkg/embedded Config.PolicySandbox), so every policy decision is a Deny")

type Host interface {
	Run(requestJSON []byte) ([]byte, error)

	Kill(runID string)

	PlantProbe(marker string) ([]byte, error)

	ClearProbe(marker string) error
}

type Request struct {
	ID string `json:"id"`

	Source string `json:"source"`

	Env map[string]string `json:"env"`

	TimeoutMs int64 `json:"timeoutMs"`
}

type Answer struct {
	ExitCode int `json:"exitCode"`

	Stdout string `json:"stdout"`

	Stderr string `json:"stderr"`

	TimedOut bool `json:"timedOut"`
}

type Runner struct {
	host Host

	timeout time.Duration

	slot chan struct{}

	next atomic.Uint64

	fenced atomic.Bool
}

func Open(_ context.Context, host Host, spec *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error) {
	if name := spec.ProviderName(); name != Kind {
		return nil, fmt.Errorf("sandbox/embedded: this host runs only the %s policy sandbox, not %s", Kind, name)
	}
	timeout := DefaultTimeout
	if spec.Embedded != nil && spec.Embedded.TimeoutSeconds != nil && *spec.Embedded.TimeoutSeconds > 0 {
		timeout = time.Duration(*spec.Embedded.TimeoutSeconds) * time.Second
	}
	return New(host, timeout), nil
}

func New(host Host, timeout time.Duration) *Runner {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Runner{host: host, timeout: timeout, slot: make(chan struct{}, 1)}
}

func (r *Runner) Kind() string { return Kind }

func (r *Runner) Available() bool { return r.host != nil }

func (r *Runner) Fenced() bool { return r.host != nil && r.fenced.Load() }

func (r *Runner) Run(ctx context.Context, p sandbox.Program) (sandbox.Result, error) {
	if r.host == nil {
		return sandbox.Result{ExitCode: -1}, ErrNoHost
	}
	if p.Dir != "" {
		return sandbox.Result{}, errors.New("sandbox/embedded: the embedded sandbox has no filesystem (Program.Dir)")
	}
	if !p.Net {
		return sandbox.Result{}, errors.New("sandbox/embedded: the embedded sandbox cannot deny the network (Program.Net=false)")
	}
	if strings.TrimSpace(p.Source) == "" {
		return sandbox.Result{}, errors.New("sandbox: the program is empty")
	}
	answer, hung, err := r.run(ctx, p)
	if err != nil || hung {
		return sandbox.Result{ExitCode: -1, TimedOut: hung}, err
	}
	return sandbox.Result{ExitCode: answer.ExitCode, TimedOut: answer.TimedOut, Stdout: capped(answer.Stdout), Stderr: capped(answer.Stderr)}, nil
}

func (r *Runner) run(ctx context.Context, p sandbox.Program) (Answer, bool, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	select {
	case r.slot <- struct{}{}:
	case <-ctx.Done():
		return Answer{}, false, ctx.Err()
	}
	id := fmt.Sprintf("r%d", r.next.Add(1))
	body, err := json.Marshal(Request{ID: id, Source: p.Source, Env: p.Env, TimeoutMs: timeout.Milliseconds()})
	if err != nil {
		<-r.slot
		return Answer{}, false, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout+Grace)
	defer cancel()
	done := call(func() ([]byte, error) { return r.host.Run(body) })
	select {
	case out := <-done:
		<-r.slot
		if out.err != nil {
			return Answer{}, false, fmt.Errorf("sandbox/embedded: run: %w", out.err)
		}
		var answer Answer
		if err := json.Unmarshal(out.raw, &answer); err != nil {
			return Answer{}, false, fmt.Errorf("sandbox/embedded: run answer: %w", err)
		}
		return answer, false, nil
	case <-runCtx.Done():
		hung := ctx.Err() == nil
		r.host.Kill(id)
		select {
		case <-done:
			<-r.slot
		case <-time.After(Grace):
			hung = true
			go func() {
				<-done
				<-r.slot
			}()
		}
		if hung {
			r.fenced.Store(false)
		}
		if ctx.Err() != nil {
			return Answer{}, false, ctx.Err()
		}
		return Answer{}, true, nil
	}
}

type hostAnswer struct {
	raw []byte
	err error
}

func call(fn func() ([]byte, error)) <-chan hostAnswer {
	done := make(chan hostAnswer, 1)
	go func() {
		raw, err := fn()
		done <- hostAnswer{raw: raw, err: err}
	}()
	return done
}

func capped(s string) string {
	if len(s) > OutputLimit {
		return s[:OutputLimit]
	}
	return s
}

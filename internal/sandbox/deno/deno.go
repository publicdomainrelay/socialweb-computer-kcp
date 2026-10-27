//go:build !js

package deno

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const (
	Kind = "deno"

	DefaultWait = 2 * time.Minute

	OutputLimit = 65536
)

var _ sandbox.Runner = (*Runner)(nil)

type Runner struct {
	Path string
}

func New(path string) *Runner {
	if path == "" {
		if found, err := exec.LookPath("deno"); err == nil {
			path = found
		}
	}
	return &Runner{Path: path}
}

func (r *Runner) Available() bool { return r.Path != "" }

func (r *Runner) Fenced() bool { return true }

func (r *Runner) Kind() string { return Kind }

func (r *Runner) Run(ctx context.Context, p sandbox.Program) (sandbox.Result, error) {
	if r.Path == "" {
		return sandbox.Result{}, errors.New("sandbox: deno is not on PATH")
	}
	if strings.TrimSpace(p.Source) == "" {
		return sandbox.Result{}, errors.New("sandbox: the program is empty")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultWait
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"run", "--quiet", "--no-prompt", "--no-config"}
	if p.Net {
		args = append(args, "--allow-net")
	} else {
		args = append(args, "--deny-net", "--no-remote", "--no-lock")
	}
	if p.Dir != "" {
		args = append(args, "--allow-read="+p.Dir, "--allow-write="+p.Dir)
	}
	args = append(args, "--deny-run", "--deny-ffi", "--ext", "ts", "-")
	cmd := exec.CommandContext(runCtx, r.Path, args...)
	if p.Dir != "" {
		cmd.Dir = p.Dir
	}
	cmd.Stdin = strings.NewReader(p.Source)
	if p.Env != nil {
		cmd.Env = envSlice(p.Env)
	}
	var out, errb capped
	out.limit, errb.limit = OutputLimit, OutputLimit
	cmd.Stdout = &out
	cmd.Stderr = &errb
	result := sandbox.Result{}
	err := cmd.Run()
	switch {
	case runCtx.Err() != nil && ctx.Err() == nil:
		result.TimedOut = true
		result.ExitCode = -1
	case cmd.ProcessState != nil:
		result.ExitCode = cmd.ProcessState.ExitCode()
	case err != nil:
		result.ExitCode = -1
	}
	if err != nil && cmd.ProcessState == nil {
		result.Stderr = strings.TrimSpace(errb.String() + "\n" + err.Error())
		return result, fmt.Errorf("sandbox: starting deno: %w", err)
	}
	result.Stdout = out.String()
	result.Stderr = errb.String()
	return result, nil
}

func envSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

type capped struct {
	limit int
	to    strings.Builder
}

func (c *capped) Write(p []byte) (int, error) {
	if left := c.limit - c.to.Len(); left > 0 {
		if len(p) > left {
			c.to.Write(p[:left])
		} else {
			c.to.Write(p)
		}
	}
	return len(p), nil
}

func (c *capped) String() string { return c.to.String() }

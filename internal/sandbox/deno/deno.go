//go:build !js

package deno

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

var (
	moduleCacheDirs = []string{"remote", "deps", "npm", "gen", "registries"}

	moduleCacheFiles = []string{"v8_code_cache_v2", "dep_analysis_cache_v2", "node_analysis_cache_v2", "check_cache_v2", "fast_check_cache_v2"}
)

var _ sandbox.Runner = (*Runner)(nil)

type Runner struct {
	Path string

	ModuleCache string
}

func New(path string) *Runner {
	if path == "" {
		if found, err := exec.LookPath("deno"); err == nil {
			path = found
		}
	}
	return &Runner{Path: path, ModuleCache: sharedModuleCache()}
}

func sharedModuleCache() string {
	if dir := strings.TrimSpace(os.Getenv("DENO_DIR")); dir != "" {
		return dir
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "deno")
	}
	return ""
}

func (r *Runner) freshState(env map[string]string) (string, []string, error) {
	root, err := os.MkdirTemp("", "deno-run-")
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: creating the run state dir: %w", err)
	}
	tmp, denoDir := filepath.Join(root, "tmp"), filepath.Join(root, "deno")
	for _, dir := range []string{tmp, denoDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			os.RemoveAll(root)
			return "", nil, fmt.Errorf("sandbox: creating the run state dir: %w", err)
		}
	}
	shared := r.ModuleCache
	if dir := strings.TrimSpace(env["DENO_DIR"]); dir != "" {
		shared = dir
	}
	if shared != "" && os.MkdirAll(shared, 0o755) == nil {
		for _, name := range moduleCacheDirs {
			if os.MkdirAll(filepath.Join(shared, name), 0o755) == nil {
				_ = os.Symlink(filepath.Join(shared, name), filepath.Join(denoDir, name))
			}
		}
		for _, name := range moduleCacheFiles {
			_ = os.Symlink(filepath.Join(shared, name), filepath.Join(denoDir, name))
		}
	}
	return root, []string{"DENO_DIR=" + denoDir, "TMPDIR=" + tmp}, nil
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
	state, stateEnv, err := r.freshState(p.Env)
	if err != nil {
		return sandbox.Result{}, err
	}
	defer os.RemoveAll(state)
	if p.Env != nil {
		cmd.Env = append(envSlice(p.Env), stateEnv...)
	} else {
		cmd.Env = append(os.Environ(), stateEnv...)
	}
	var out, errb capped
	out.limit, errb.limit = OutputLimit, OutputLimit
	cmd.Stdout = &out
	cmd.Stderr = &errb
	result := sandbox.Result{}
	err = cmd.Run()
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

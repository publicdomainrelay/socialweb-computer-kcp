package bidder

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fmt"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	SandboxProgram       = "main.ts"
	SandboxOutputLimit   = 65536
	SandboxDefaultWait   = 2 * time.Minute
	SandboxAddressPrefix = "sandbox://"
)

type sandboxRun struct {
	name   string
	cancel context.CancelFunc
	done   chan struct{}
}

func (b *Bidder) sandboxDir(name string) string {
	return filepath.Join(b.cfg.StateDir, "sandboxes", name)
}

func sandboxAddress(name string) string { return SandboxAddressPrefix + name }

func (b *Bidder) startSandbox(ctx context.Context, vm *v1alpha1.ComputeVM) error {
	if b.cfg.DenoPath == "" {
		return errors.New("bidder: this host has no deno on PATH, so the deno compute kind cannot run")
	}
	if vm.Spec.Sandbox == nil || strings.TrimSpace(vm.Spec.Sandbox.Source) == "" {
		return errors.New("bidder: the sandbox carries no program")
	}
	b.sandboxMu.Lock()
	if _, running := b.sandboxes[vm.Name]; running {
		b.sandboxMu.Unlock()
		return nil
	}
	dir := b.sandboxDir(vm.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.sandboxMu.Unlock()
		return err
	}
	program := filepath.Join(dir, SandboxProgram)
	if err := os.WriteFile(program, []byte(vm.Spec.Sandbox.Source), 0o600); err != nil {
		b.sandboxMu.Unlock()
		return err
	}
	timeout := time.Duration(vm.Spec.Sandbox.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = SandboxDefaultWait
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	run := &sandboxRun{name: vm.Name, cancel: cancel, done: make(chan struct{})}
	b.sandboxes[vm.Name] = run
	b.sandboxMu.Unlock()
	b.log.Info("a deno sandbox starts", "computevm", vm.Name, "dir", dir, "deno", b.cfg.DenoPath, "timeout", timeout.String())
	go b.runSandbox(runCtx, vm.Name, program, dir)
	return nil
}

func (b *Bidder) runSandbox(ctx context.Context, name, program, dir string) {
	var out, errb strings.Builder
	cmd := exec.CommandContext(ctx, b.cfg.DenoPath, "run", "--no-remote", "--no-lock", "--quiet",
		"--allow-read="+dir, "--allow-write="+dir, "--deny-net", "--deny-run", "--deny-ffi", program)
	cmd.Dir = dir
	cmd.Stdout = &capped{limit: SandboxOutputLimit, to: &out}
	cmd.Stderr = &capped{limit: SandboxOutputLimit, to: &errb}
	startErr := cmd.Start()
	exitCode := int64(0)
	timedOut := false
	if startErr != nil {
		exitCode = -1
		errb.WriteString(startErr.Error())
	} else {
		_ = b.publishSandboxOnNetwork(ctx, name)
		waitErr := cmd.Wait()
		exitCode = int64(cmd.ProcessState.ExitCode())
		if ctx.Err() != nil {
			timedOut = true
			exitCode = -1
		} else if waitErr != nil && exitCode == 0 {
			exitCode = -1
		}
	}
	if exitCode == 0 && out.Len() == 0 && errb.Len() == 0 && startErr == nil {
		exitCode = 0
	}
	b.log.Info("a deno sandbox ended", "computevm", name, "exit", exitCode, "timedOut", timedOut, "stdoutBytes", out.Len(), "stderrBytes", errb.Len())
	if err := b.publishSandboxResult(context.WithoutCancel(ctx), name, exitCode, timedOut, out.String(), errb.String()); err != nil {
		b.log.Warn("publishing the sandbox result failed", "computevm", name, "err", err.Error())
	}
	b.sandboxMu.Lock()
	delete(b.sandboxes, name)
	b.sandboxMu.Unlock()
}

func (b *Bidder) publishSandboxOnNetwork(ctx context.Context, name string) error {
	vm, ok := b.vms.Get(name)
	if !ok {
		return fmt.Errorf("bidder: the sandbox %s is not in the index", name)
	}
	record := map[string]any{"$type": market.ComputeEventsVMOnNetworkNSID, "kind": "sandbox", "address": sandboxAddress(name),
		"createdAt": b.now().UTC().Format(time.RFC3339)}
	_, err := b.ensureReportSync(ctx, &vm, market.ComputeEventsVMOnNetworkNSID, "", record)
	return err
}

func (b *Bidder) publishSandboxResult(ctx context.Context, name string, exitCode int64, timedOut bool, stdout, stderr string) error {
	vm, ok := b.vms.Get(name)
	if !ok {
		return fmt.Errorf("bidder: the sandbox %s is not in the index", name)
	}
	record := map[string]any{"$type": market.ComputeEventsVMSandboxNSID, "exitCode": exitCode, "timedOut": timedOut,
		"stdout": stdout, "stderr": stderr, "createdAt": b.now().UTC().Format(time.RFC3339)}
	name2, err := b.ensureReportSync(ctx, &vm, market.ComputeEventsVMSandboxNSID, "", record)
	if err == nil {
		b.log.Info("a deno sandbox result is published to the requester", "computevm", name, "stateSync", name2)
	}
	return err
}

func (b *Bidder) stopSandbox(name string) {
	b.sandboxMu.Lock()
	run, ok := b.sandboxes[name]
	b.sandboxMu.Unlock()
	if !ok {
		return
	}
	run.cancel()
	b.log.Info("a deno sandbox is stopped", "computevm", name)
}

type capped struct {
	limit int
	to    *strings.Builder
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

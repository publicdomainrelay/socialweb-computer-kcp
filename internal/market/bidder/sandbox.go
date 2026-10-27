package bidder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const (
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
	if b.cfg.Sandbox == nil || !b.cfg.Sandbox.Available() {
		return errors.New("bidder: this host has no sandbox runner, so the deno compute kind cannot run")
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
	timeout := time.Duration(vm.Spec.Sandbox.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = SandboxDefaultWait
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &sandboxRun{name: vm.Name, cancel: cancel, done: make(chan struct{})}
	b.sandboxes[vm.Name] = run
	b.sandboxMu.Unlock()
	b.log.Info("a deno sandbox starts", "computevm", vm.Name, "dir", dir, "runner", b.cfg.Sandbox.Kind(), "timeout", timeout.String())
	program := sandbox.Program{Source: vm.Spec.Sandbox.Source, Dir: dir, Timeout: timeout}
	go b.runSandbox(runCtx, vm.Name, program)
	return nil
}

func (b *Bidder) runSandbox(ctx context.Context, name string, program sandbox.Program) {
	b.sandboxMu.Lock()
	run := b.sandboxes[name]
	b.sandboxMu.Unlock()
	defer func() {
		b.sandboxMu.Lock()
		delete(b.sandboxes, name)
		b.sandboxMu.Unlock()
		if run != nil {
			close(run.done)
		}
	}()
	if err := b.publishSandboxOnNetwork(ctx, name); err != nil {
		b.log.Warn("publishing the sandbox on-network event failed", "computevm", name, "err", err.Error())
	}
	result, err := b.cfg.Sandbox.Run(ctx, program)
	if err != nil && ctx.Err() == nil {
		b.log.Warn("the sandbox runner failed", "computevm", name, "err", err.Error())
	}
	b.log.Info("a deno sandbox ended", "computevm", name, "exit", result.ExitCode, "timedOut", result.TimedOut,
		"stdoutBytes", len(result.Stdout), "stderrBytes", len(result.Stderr))
	if err := b.publishSandboxResult(context.WithoutCancel(ctx), name, int64(result.ExitCode), result.TimedOut, result.Stdout, result.Stderr); err != nil {
		b.log.Warn("publishing the sandbox result failed", "computevm", name, "err", err.Error())
	}
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

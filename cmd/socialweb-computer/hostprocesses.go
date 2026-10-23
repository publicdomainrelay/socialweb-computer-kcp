package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const hostProcessesApplyTimeout = time.Minute

func runHostProcesses(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	for _, n := range []string{backendOpt, "instance", "state-dir", "kcp-kubeconfig"} {
		if err := a.spec.Require(vals, n); err != nil {
			return a.fail(exitRefused, "internal hostprocesses: %v", err)
		}
	}
	backend, err := hostsvc.ParseBackend(vals.String(backendOpt))
	if err != nil {
		return a.fail(exitRefused, "internal hostprocesses: %v", err)
	}
	if backend != hostsvc.BackendSystemdUser {
		return a.fail(exitRefused, "internal hostprocesses: only the systemd-user backend; the proc backend belongs to a dev instance's devsup")
	}
	stateDir := vals.String("state-dir")
	if !filepath.IsAbs(stateDir) {
		return a.fail(exitRefused, "internal hostprocesses: state-dir %q is not absolute", stateDir)
	}
	kubeconfig, ws, bao := vals.String("kcp-kubeconfig"), vals.String("kcp-base-workspace"), vals.String("bao-path")
	if bao != "" && !filepath.IsAbs(bao) {
		return a.fail(exitRefused, "internal hostprocesses: bao-path %q is not absolute", bao)
	}
	m, err := hostsvc.Open(ctx, hostsvc.Config{Backend: backend})
	if err != nil {
		return a.fail(exitFailed, "internal hostprocesses: %v", err)
	}
	defer m.Close()
	prefix := table.DevNames(vals.String("instance")).HostProcessPrefix()
	log := a.log.With("component", "hostprocesses", "instance", vals.String("instance"), "workspace", ws)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg.Go(func() {
		openbaoctl.Supervise(ctx, openbaoctl.Config{Kubeconfig: kubeconfig, Workspace: ws, StateDir: stateDir, Prefix: prefix, Log: log})
	})
	hostprocess.Supervise(ctx, hostprocess.Config{Kubeconfig: kubeconfig, Workspace: ws, Manager: m, Prefix: prefix,
		Resolve: func(asset v1alpha1.HostProcessAsset) (string, error) {
			if asset.Name == "bao" && bao != "" {
				return bao, nil
			}
			return "", fmt.Errorf("internal hostprocesses: no executable %s (bao-path names bao)", asset.Name)
		},
		ApplyTimeout: hostProcessesApplyTimeout, Log: log})
	return exitOK
}

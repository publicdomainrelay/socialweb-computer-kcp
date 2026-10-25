package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

var installOnlyChecks = []string{hostcontract.CheckAppArmor, hostcontract.CheckStateACL}

var systemdChecks = []string{hostcontract.CheckSystemdVersion, hostcontract.CheckProbeProperties, hostcontract.CheckCgroupV2,
	hostcontract.CheckDelegation, hostcontract.CheckLinger, hostcontract.CheckSocketPath, hostcontract.CheckManagerKVM}

func (a *app) vmHostPreflight(ctx context.Context, vals clicfg.Values) error {
	backend := hostsvc.BackendProc
	if b := vals.String("vm-host-backend"); b != "" {
		parsed, err := hostsvc.ParseBackend(b)
		if err != nil {
			return err
		}
		backend = parsed
	}
	exe, err := selfExe()
	if err != nil {
		return err
	}
	state, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return err
	}
	cache, err := a.assetCache(vals)
	if err != nil {
		return err
	}
	getenv := func(k string) string {
		v, _ := a.env.LookupEnv(k)
		return v
	}
	results := hostcontract.Evaluate(hostcontract.Gather(ctx, hostcontract.Options{
		Role: hostcontract.RoleBidder, StateDir: state, Self: []string{exe, "check-host"}, PastaPath: vals.String("pasta-path"),
		AssetCache: cache, Getenv: getenv,
	}))
	var lines []string
	for _, r := range results {
		if r.Status != hostcontract.StatusFail || slices.Contains(installOnlyChecks, r.Name) ||
			(backend != hostsvc.BackendSystemdUser && slices.Contains(systemdChecks, r.Name)) {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %s\n    fix: %s", r.Name, r.Detail, r.Fix))
	}
	if len(lines) > 0 {
		return fmt.Errorf("this host cannot run bidder VMs as this account; socialweb-computer check-host -role bidder -state-dir %s lists every check:\n%s",
			state, strings.Join(lines, "\n"))
	}
	return nil
}

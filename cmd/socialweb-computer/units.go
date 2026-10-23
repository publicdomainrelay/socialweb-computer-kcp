package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc/treeaudit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpdatastore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

type placement struct {
	manager hostsvc.Manager
	devsup  int
	runtime string
}

func (a *app) placement(ctx context.Context, vals clicfg.Values) (placement, error) {
	backend, err := hostsvc.ParseBackend(vals.String(backendOpt))
	if err != nil {
		return placement{}, err
	}
	p := placement{runtime: vals.String("asset-cache")}
	switch backend {
	case hostsvc.BackendSystemdUser:
		if p.runtime == "" && vals.String("state-dir") != "" {
			p.runtime = filepath.Join(vals.String("state-dir"), "runtime")
		}
		if p.runtime == "" {
			return p, fmt.Errorf("the systemd-user backend needs --state-dir or --asset-cache to find the pinned runtime sets (install writes both into the account's config)")
		}
		p.manager, err = hostsvc.Open(ctx, hostsvc.Config{Backend: backend})
		return p, err
	case hostsvc.BackendProc:
		l, err := a.layout(vals)
		if err != nil {
			return p, err
		}
		c, h, err := helloDevsup(ctx, l)
		if err != nil {
			return p, fmt.Errorf("the instance's devsup at %s: %w", l.Socket, err)
		}
		p.manager, p.devsup = c, h.PID
		if p.runtime == "" {
			if p.runtime, err = assets.DevCache(a.env); err != nil {
				return p, err
			}
		}
		return p, nil
	}
	return p, fmt.Errorf("no placement backend %q", backend)
}

func runUnitsList(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	p, err := a.placement(ctx, vals)
	if err != nil {
		return a.fail(exitRefused, "units list: %v", err)
	}
	defer p.manager.Close()
	var all []hostsvc.UnitStatus
	seen := map[string]bool{}
	for _, prefix := range []string{"socialweb-vm-", "socialweb-market-network-", "socialweb-bao-", "socialweb-kcp-", "socialweb-hp-", "socialweb-pg-", "socialweb-c-", "socialweb-computer-", "socialweb-probe-"} {
		st, err := p.manager.List(ctx, prefix)
		if err != nil {
			return a.fail(exitFailed, "units list: %v", err)
		}
		for _, s := range st {
			if !seen[s.Name] {
				seen[s.Name] = true
				all = append(all, s)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	if vals.Bool("json") {
		a.printJSON(all)
		return exitOK
	}
	for _, s := range all {
		fmt.Fprintf(a.stdout, "%-56s %-12s %-10s pid %d\n", s.Name, s.State, s.SubState, s.MainPID)
	}
	return exitOK
}

func runUnitsRollKcp(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if len(rest) != 0 {
		return a.fail(exitRefused, "units roll kcp: unexpected arguments %v", rest)
	}
	if err := devinstance.RefuseRoot(os.Geteuid()); err != nil {
		return a.fail(exitRefused, "units roll kcp: %v", err)
	}
	backend, err := hostsvc.ParseBackend(vals.String(backendOpt))
	if err != nil {
		return a.fail(exitRefused, "units roll kcp: %v", err)
	}
	if backend != hostsvc.BackendSystemdUser {
		return a.fail(exitRefused, "units roll kcp: the installed kcp units are user units (--%s %s); a dev instance rolls with dev roll kcp", backendOpt, hostsvc.BackendSystemdUser)
	}
	state := vals.String("state-dir")
	if state == "" || !filepath.IsAbs(state) {
		return a.fail(exitRefused, "units roll kcp: --state-dir must name the installed state root (an absolute path)")
	}
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "units roll kcp: ready-timeout: %v", err)
	}
	retention, err := kcpdatastore.RetentionOf(vals.Int("store-backup-keep"), vals.Int("store-backup-keep-days"))
	if err != nil {
		return a.fail(exitRefused, "units roll kcp: %v", err)
	}
	app := table.Gateway()
	if h := vals.String("handle"); h != "" {
		app = table.Bidder(h, install.HandleID(h))
	}
	prefix := vals.String("unit-prefix")
	if prefix != "" {
		if err := hostsvc.ValidatePrefix(prefix); err != nil {
			return a.fail(exitRefused, "units roll kcp: unit-prefix: %v", err)
		}
		app.Prefix = prefix
	}
	compat, err := versions.KcpCompatRecords()
	if err != nil {
		return a.fail(exitFailed, "units roll kcp: %v", err)
	}
	p := bootstrap.InstallParams(app, state)
	lock, err := lockFile(ctx, p.RollLock(), rollLockWait)
	if err != nil {
		return a.fail(exitFailed, "units roll kcp: %v", err)
	}
	defer lock.Close()
	m, err := hostsvc.Open(ctx, hostsvc.Config{Backend: backend, UnitPrefix: prefix})
	if err != nil {
		return a.fail(exitFailed, "units roll kcp: %v", err)
	}
	defer m.Close()
	rep, err := bootstrap.RollInstalled(ctx, m, p, bootstrap.InstalledRoll{Kcp: vals.String("kcp-path"), Kine: vals.String("kine-path"),
		PlannedOutage: vals.Bool("planned-outage"), Retention: retention, Compat: compat, ReadyTimeout: readyTimeout, Log: a.log})
	a.printJSON(rep)
	if errors.Is(err, kcpserver.ErrIncompatiblePair) {
		return a.fail(exitRefused, "units roll kcp: %v", err)
	}
	if err != nil {
		return a.fail(exitFailed, "units roll kcp: %v", err)
	}
	return exitOK
}

func runUnitsAudit(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	p, err := a.placement(ctx, vals)
	if err != nil {
		return a.fail(exitRefused, "units audit: %v", err)
	}
	defer p.manager.Close()
	pins, err := versions.PlatformPins(assets.Platform())
	if err != nil {
		return a.fail(exitFailed, "units audit: %v", err)
	}
	pinned, notes := treeaudit.Pinned(p.runtime, pins, assets.TreeDigest)
	for _, n := range notes {
		fmt.Fprintln(a.stderr, "units audit: "+n)
	}
	r, err := treeaudit.Audit(ctx, p.manager, treeaudit.Host{}, p.devsup, pinned)
	if err != nil {
		return a.fail(exitFailed, "units audit: %v", err)
	}
	if vals.Bool("json") {
		a.printJSON(r)
	} else {
		treeaudit.Write(a.stdout, r)
	}
	if !r.Pass() {
		return exitFailed
	}
	return exitOK
}

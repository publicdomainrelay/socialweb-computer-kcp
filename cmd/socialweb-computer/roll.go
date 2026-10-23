package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpdatastore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const rollLockWait = 2 * time.Minute

type rollReport struct {
	Instance string               `json:"instance"`
	Roll     kcpserver.RollReport `json:"roll"`
	Slots    kcpserver.Slots      `json:"slots"`
	Units    []devinstance.Unit   `json:"units"`
	Error    string               `json:"error,omitempty"`
}

func runDevRoll(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if len(rest) != 0 {
		return a.fail(exitRefused, "dev roll kcp: unexpected arguments %v", rest)
	}
	if err := devinstance.RefuseRoot(os.Geteuid()); err != nil {
		return a.fail(exitRefused, "dev roll kcp: %v", err)
	}
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "dev roll kcp: ready-timeout: %v", err)
	}
	retention, err := kcpdatastore.RetentionOf(vals.Int("store-backup-keep"), vals.Int("store-backup-keep-days"))
	if err != nil {
		return a.fail(exitRefused, "dev roll kcp: %v", err)
	}
	l, err := a.layout(vals)
	if err != nil {
		return a.fail(exitRefused, "dev roll kcp: %v", err)
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		return a.fail(exitRefused, "dev roll kcp: no instance %s here (dev up first): %v", l.ID, err)
	}
	kcpExe := rec.Executables["kcp"]
	if now, err := checkExe(ctx, "kcp-path", kcpExe.Path); err != nil {
		return a.fail(exitRefused, "dev roll kcp: the recorded kcp executable: %v", err)
	} else if compat, err := versions.KcpCompatRecords(); err != nil {
		return a.fail(exitFailed, "dev roll kcp: %v", err)
	} else if _, err := kcpserver.CheckPair(kcpserver.Asset(kcpExe), kcpserver.Asset(now), compat, false); err != nil {
		return a.fail(exitRefused, "dev roll kcp: %s changed from %q to %q: %v", kcpExe.Path, kcpExe.Version, now.Version, err)
	}
	lock, err := lockFile(l.Lock, rollLockWait)
	if err != nil {
		return a.fail(exitFailed, "dev roll kcp: %v", err)
	}
	defer lock.Close()
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		return a.fail(exitFailed, "dev roll kcp: %v", err)
	}
	m, _, err := helloDevsup(ctx, l)
	if err != nil {
		return a.fail(exitFailed, "dev roll kcp: devsup of %s unreachable: %v", l.ID, err)
	}
	defer m.Close()
	bcfg := bootstrap.Config{Layout: l, KcpPath: kcpExe.Path, KinePath: rec.Executables["kine"].Path, PostgresRoot: rec.Executables["postgres"].Path,
		Exe: rec.Executables["socialweb-computer"].Path, ReadyTimeout: readyTimeout, Retention: retention, Log: a.log}
	rep, rerr := bootstrap.Roll(ctx, m, bcfg, &rec)
	werr := fsx.WriteJSON(l.Record, rec, 0o600)
	out := rollReport{Instance: l.ID, Roll: rep, Units: rec.Units}
	out.Slots, _ = kcpserver.ReadSlots(l.SlotsFile())
	if err := errors.Join(rerr, werr); err != nil {
		out.Error = err.Error()
		a.printJSON(out)
		return a.fail(exitFailed, "dev roll kcp: %v", err)
	}
	a.printJSON(out)
	return exitOK
}

func runApiserverFront(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	for _, n := range []string{"owner", "kcp-front-address", "front-control", "front-state"} {
		if err := a.spec.Require(vals, n); err != nil {
			return a.fail(exitRefused, "internal apiserver-front: %v", err)
		}
	}
	err := apiserverfront.Run(ctx, apiserverfront.Config{Owner: vals.String("owner"), Address: vals.String("kcp-front-address"),
		Control: vals.String("front-control"), State: vals.String("front-state"), FDStore: vals.Bool("front-fd-store"), Log: a.log})
	if err != nil {
		return a.fail(exitFailed, "internal apiserver-front: %v", err)
	}
	return exitOK
}

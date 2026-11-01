package request

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devcore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/notify"
)

const (
	ExeKcp  = "kcp"
	ExeKine = "kine"
)

type CoreConfig struct {
	StateDir     string
	Prefix       string
	Name         string
	Executables  map[string]devinstance.Executable
	FrontAddress string
	ReadyTimeout time.Duration
	Log          *slog.Logger
	Serve        func(ctx context.Context, rec devinstance.Record, ready func() error) error
}

func CoreLayout(state, prefix, name string) devinstance.Layout {
	return install.RequesterLayout(state, prefix, name)
}

func (c CoreConfig) app() table.App {
	a := table.Requester(c.Name, install.RequesterKey(c.Name))
	if c.Prefix != "" {
		a.Prefix = c.Prefix
	}
	return a
}

func (c CoreConfig) record(l devinstance.Layout) (devinstance.Record, error) {
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		rec = devinstance.Record{Created: time.Now().UTC()}
	}
	p := bootstrap.InstallParams(c.app(), c.StateDir)
	exes := map[string]devinstance.Executable{}
	maps.Copy(exes, c.Executables)
	if kcp, kine := c.Executables[ExeKcp].Path, c.Executables[ExeKine].Path; kcp != "" && kine != "" {
		if err := os.MkdirAll(filepath.Dir(p.KcpPath), 0o700); err != nil {
			return rec, err
		}
		if err := bootstrap.LinkAssets(p, kcp, kine, ""); err != nil {
			return rec, err
		}
	}
	for name, link := range map[string]string{ExeKcp: p.KcpPath, ExeKine: p.KinePath} {
		if _, err := os.Stat(link); err != nil {
			return rec, fmt.Errorf("requester: %s is not linked at %s (install links it; a dev build names it with its -path option): %w", name, link, err)
		}
		e := exes[name]
		e.Path = link
		exes[name] = e
	}
	rec.Layout, rec.Profile, rec.Backend, rec.Executables = l, bootstrap.ProfileQuick, hostsvc.BackendSystemdUser, exes
	rec.Role = devinstance.RoleRequester
	rec.BaseWorkspace, rec.Host, rec.Parent, rec.StandIns, rec.OpenBao, rec.VM = "", false, nil, nil, nil, nil
	return rec, nil
}

func RunCore(ctx context.Context, c CoreConfig, m hostsvc.Manager) error {
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("requester: state dir %q is not absolute", c.StateDir)
	}
	if err := CheckName(c.Name); err != nil {
		return err
	}
	l := CoreLayout(c.StateDir, c.Prefix, c.Name)
	for _, d := range []string{l.StateDir, l.KcpDir(), l.RuntimeDir, l.Logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if n := len(filepath.Join(l.RuntimeDir, "kine-a.sock")); n > devinstance.MaxSocketPath {
		return fmt.Errorf("requester: the runtime dir %s makes socket paths of %d bytes, over %d", l.RuntimeDir, n, devinstance.MaxSocketPath)
	}
	rec, err := c.record(l)
	if err != nil {
		return err
	}
	if err := fsx.WriteJSON(l.Record, rec, 0o600); err != nil {
		return err
	}
	app := c.app()
	log := c.Log.With("component", "run-requester", "name", c.Name)
	log.Info("the requester application instance starts: kcp store, kine, front, its slot, then its MarketRequester root object",
		"state", l.StateDir, "units", app.Names().UnitPrefix())
	var watchdogLogged atomic.Bool
	go notify.KeepAlive(ctx, func(err error) {
		if watchdogLogged.CompareAndSwap(false, true) {
			log.Warn("telling systemd's watchdog the core is alive", "err", err.Error())
		}
	})
	ready := func() error {
		if err := notify.Ready(); err != nil {
			return fmt.Errorf("requester: telling systemd the requester is ready: %w", err)
		}
		return nil
	}
	return devcore.Run(ctx, devcore.Config{Layout: l, ReadyTimeout: c.ReadyTimeout, Log: c.Log, Application: &app, FrontAddress: c.FrontAddress,
		OnStep: func(step string) { log.Info("the core brings up the stack", "step", step) },
		OnTick: func() {
			if err := notify.Watchdog(); err != nil && watchdogLogged.CompareAndSwap(false, true) {
				log.Warn("telling systemd's watchdog the core is alive", "err", err.Error())
			}
		},
		Members: func(ctx context.Context, rec devinstance.Record) error {
			return c.Serve(ctx, rec, ready)
		}}, m)
}

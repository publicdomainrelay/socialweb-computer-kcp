package bidder

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devcore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/notify"
)

const (
	ExeCore           = "socialweb-computer"
	ExeKcp            = "kcp"
	ExeKine           = "kine"
	ExeBao            = "bao"
	ApplyTimeout      = time.Minute
	RootApplyDeadline = 3 * time.Minute
	RootRetryLogEvery = time.Minute
)

type CoreConfig struct {
	StateDir     string
	Prefix       string
	Handle       string
	Executables  map[string]devinstance.Executable
	Spec         v1alpha1.MarketBidderSpec
	VM           devinstance.VMRuntime
	ComputeVM    computevm.Config
	FrontAddress string
	ReadyTimeout time.Duration
	Unit         string
	Log          *slog.Logger
}

func NewLayout(state, prefix, handle string) devinstance.Layout {
	return install.BidderLayout(state, prefix, handle)
}

func (c CoreConfig) app() table.App {
	a := table.Bidder(c.Handle, install.HandleID(c.Handle))
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
		if err := bootstrap.LinkAssets(p, kcp, kine, ""); err != nil {
			return rec, err
		}
	}
	for name, link := range map[string]string{ExeKcp: p.KcpPath, ExeKine: p.KinePath} {
		if _, err := os.Stat(link); err != nil {
			return rec, fmt.Errorf("bidder: %s is not linked at %s (install links it; a dev build names it with its -path option): %w", name, link, err)
		}
		e := exes[name]
		e.Path = link
		exes[name] = e
	}
	vm := c.VM
	rec.Layout, rec.Profile, rec.Backend, rec.Executables = l, bootstrap.ProfileQuick, hostsvc.BackendSystemdUser, exes
	rec.BaseWorkspace, rec.Host, rec.Parent, rec.StandIns, rec.OpenBao, rec.VM = "", false, nil, nil, nil, &vm
	return rec, nil
}

func RunCore(ctx context.Context, c CoreConfig, m hostsvc.Manager) error {
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("bidder: state dir %q is not absolute", c.StateDir)
	}
	l := NewLayout(c.StateDir, c.Prefix, c.Handle)
	for _, d := range []string{l.StateDir, l.KcpDir(), l.RuntimeDir, l.Logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if n := len(filepath.Join(l.RuntimeDir, "kine-a.sock")); n > devinstance.MaxSocketPath {
		return fmt.Errorf("bidder: the runtime dir %s makes socket paths of %d bytes, over %d", l.RuntimeDir, n, devinstance.MaxSocketPath)
	}
	rec, err := c.record(l)
	if err != nil {
		return err
	}
	if err := fsx.WriteJSON(l.Record, rec, 0o600); err != nil {
		return err
	}
	app := c.app()
	log := c.Log.With("component", "run-bidder", "handle", c.Handle)
	log.Info("the bidder application instance starts: kcp store, kine, front, its slot, then its MarketBidder root object",
		"state", l.StateDir, "units", app.Names().UnitPrefix(), "profile", c.Spec.Profile)
	cvm := c.ComputeVM
	watchdogLogged := false
	return devcore.Run(ctx, devcore.Config{Layout: l, ReadyTimeout: c.ReadyTimeout, Log: c.Log, Application: &app,
		FrontAddress: c.FrontAddress, ComputeVM: &cvm, SignInGuard: CheckSignIn,
		OnStep: func(step string) { log.Info("the core brings up the stack", "step", step) },
		OnTick: func() {
			if err := notify.Watchdog(); err != nil && !watchdogLogged {
				watchdogLogged = true
				log.Warn("telling systemd's watchdog the core is alive", "err", err.Error())
			}
		},
		Members: func(ctx context.Context, rec devinstance.Record) error {
			k, err := OpenKcp(rec.AdminKubeconfig)
			if err != nil {
				return err
			}
			if err := c.applyRoot(ctx, k, log); err != nil {
				return err
			}
			if err := notify.Ready(); err != nil {
				return fmt.Errorf("bidder: telling systemd the core is ready: %w", err)
			}
			<-ctx.Done()
			return nil
		}}, m)
}

func attempts(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

func (c CoreConfig) applyRoot(ctx context.Context, k *Kcp, log *slog.Logger) error {
	started := time.Now()
	last := started
	var lastErr error
	for attempt := 1; ; attempt++ {
		remaining := RootApplyDeadline - time.Since(started)
		if remaining <= 0 {
			return fmt.Errorf("bidder: the MarketBidder root object is not applied after %s and %s: %w",
				time.Since(started).Round(time.Second), attempts(attempt-1), lastErr)
		}
		actx, cancel := context.WithTimeout(ctx, remaining)
		err := k.ApplyRoot(actx, c.Spec)
		cancel()
		if err == nil {
			log.Info("the MarketBidder root object is applied; the core renders its components", "profile", c.Spec.Profile,
				"attempts", attempt, "after", time.Since(started).Round(time.Second).String())
			return nil
		}
		lastErr = err
		if attempt == 1 || time.Since(last) >= RootRetryLogEvery {
			log.Warn("applying the MarketBidder root object; still retrying",
				"attempt", attempt, "elapsed", time.Since(started).Round(time.Second).String(),
				"deadline", RootApplyDeadline.String(), "err", err.Error())
			last = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Poll):
		}
	}
}

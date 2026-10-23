package devcore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketbidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketnetwork"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/logcap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/postgresctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/componentcore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/storemaint"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	CheckInterval  = 5 * time.Second
	recordLockWait = 2 * time.Minute
)

var TTLSkipWorkspaces = []string{"socialweb-provider"}

func TTLScope(root string) func(workspace string) bool {
	return func(workspace string) bool {
		if workspace == root {
			return true
		}
		rest, ok := strings.CutPrefix(workspace, root+":")
		return ok && !strings.Contains(rest, ":") && !slices.Contains(TTLSkipWorkspaces, rest)
	}
}

type Config struct {
	Layout       devinstance.Layout
	ReadyTimeout time.Duration
	Log          *slog.Logger
	TLSCertFile  string
	TLSKeyFile   string
	ComputeVM    *computevm.Config
	Application  *table.App
	FrontAddress string
	OnStep       func(step string)
	OnTick       func()
	Before       func(ctx context.Context, rec devinstance.Record) error
	Members      func(ctx context.Context, rec devinstance.Record) error
	Listeners    func(owner string) []v1alpha1.ApplicationListener
	WebApp       *gateway.WebApp
	OnComponents func(owner string, listeners []v1alpha1.ApplicationListener)
	SignInGuard  marketbidder.SignInGuard
	Handlers     *component.HandlerRegistry
	RemotePDS    *pdsv1alpha1.RemotePDS

	NodeClaimAgent *kcpclient.ServiceAccountRef
}

func LogCapConfig(l devinstance.Layout, log *slog.Logger) logcap.Config {
	c := logcap.Config{Dirs: []string{l.Logs}, Log: log}
	if l.SupState != "" {
		c.Dirs = append(c.Dirs, filepath.Join(l.SupState, "logs"))
		c.Files = append(c.Files, filepath.Join(l.SupState, "devsup.log"))
	}
	return c
}

func tryLockRecord(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

func lockRecord(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err == nil {
		return f, nil
	}
	locked := make(chan error, 1)
	go func() { locked <- hostos.Lock(f, hostos.LockExclusive) }()
	timer := time.NewTimer(recordLockWait)
	defer timer.Stop()
	select {
	case err := <-locked:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("devcore: locking %s: %w", path, err)
		}
		return f, nil
	case <-timer.C:
		go func() {
			<-locked
			f.Close()
		}()
		return nil, fmt.Errorf("devcore: %s stayed locked", path)
	}
}

func Run(ctx context.Context, cfg Config, m hostsvc.Manager) error {
	l := cfg.Layout
	log := cfg.Log.With("component", "dev-core", "instance", l.ID)
	handlers := cfg.Handlers
	if handlers == nil {
		handlers = component.NewHandlerRegistry()
	}
	cfg.Handlers = handlers
	lf, err := os.OpenFile(l.CoreLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := hostos.Lock(lf, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		return fmt.Errorf("devcore: another core holds %s: %w", l.CoreLock, err)
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		return fmt.Errorf("devcore: reading %s: %w", l.Record, err)
	}
	var prev devinstance.Core
	fsx.ReadJSON(l.CoreRecord, &prev)
	core := devinstance.Core{Generation: prev.Generation + 1, PID: os.Getpid(), InvocationID: os.Getenv("INVOCATION_ID"),
		StartedAt: time.Now().UTC()}
	bcfg := bootstrap.Config{Layout: l, KcpPath: rec.Executables["kcp"].Path, KinePath: rec.Executables["kine"].Path, PostgresRoot: rec.Executables["postgres"].Path,
		Exe: rec.Executables["socialweb-computer"].Path, ReadyTimeout: cfg.ReadyTimeout, Application: cfg.Application, FrontAddress: cfg.FrontAddress,
		OnStep: cfg.OnStep, OnTick: cfg.OnTick, Log: log}
	lk, err := lockRecord(l.Lock)
	if err != nil {
		return err
	}
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		lk.Close()
		return fmt.Errorf("devcore: reading %s: %w", l.Record, err)
	}
	err = ensure(ctx, m, bcfg, l, &rec, &core, log)
	if err == nil && cfg.Application != nil && rec.Backend == hostsvc.BackendSystemdUser && rec.SharedKcp == "" {
		err = rollDrifted(ctx, m, bcfg, cfg, l, &rec, &core, log)
	}
	lk.Close()
	if err != nil {
		return err
	}
	if cfg.Before != nil {
		if err := cfg.Before(ctx, rec); err != nil {
			return err
		}
	}
	g := supervise.New(ctx)
	defer g.Stop()
	ctx = g.Context()
	logCfg := LogCapConfig(l, log)
	g.Go("logcap", func(ctx context.Context) error {
		logcap.Keep(ctx, logCfg)
		return nil
	})
	if len(rec.StandIns) == 0 && rec.AdminKubeconfig != "" {
		ac := AppConfig(bcfg, rec, m, log)
		ac.Bus, ac.RemotePDS = cfg.Handlers, cfg.RemotePDS
		ac.Listeners, ac.OnComponents = cfg.Listeners, cfg.OnComponents
		var ttlReport atomic.Pointer[func() *v1alpha1.TTLStatus]
		ac.TTLReport = func() *v1alpha1.TTLStatus {
			if report := ttlReport.Load(); report != nil {
				return (*report)()
			}
			return nil
		}
		coreCfg := ComponentCoreConfig(ctx, cfg, rec, log)
		coreCfg.Postgres = postgresctl.New(PostgresConfig(bcfg, rec))
		if servers, err := openbaoctl.New(OpenBaoServerConfig(bcfg, rec, log)); err != nil {
			log.Warn("the OpenBaoServer controller does not start", "err", err.Error())
		} else {
			coreCfg.OpenBao = servers
		}
		coreCfg.TTLScope = TTLScope(cmp.Or(ac.Workspace, hostprocess.DefaultWorkspace))
		coreCfg.OnTTL = func(report func() *v1alpha1.TTLStatus) { ttlReport.Store(&report) }
		if rec.SharedKcp == "" {
			if sc, err := StoreMaintConfig(bcfg, rec, log); err != nil {
				log.Warn("the store maintenance does not start", "err", err.Error())
			} else {
				mon := storemaint.New(sc)
				ac.StoreUsage = mon.Usage
				g.Go("storemaint", func(ctx context.Context) error {
					mon.Run(ctx)
					return nil
				})
			}
		}
		coreCfg.Apps = &app.Config{Compose: ac, WebApp: cfg.WebApp, SignInGuard: cfg.SignInGuard}
		g.Go("componentcore", func(ctx context.Context) error {
			componentcore.Supervise(ctx, coreCfg)
			return nil
		})
		hpCfg := HostProcessConfig(bcfg, rec, m, log)
		g.Go("hostprocess", func(ctx context.Context) error {
			hostprocess.Supervise(ctx, hpCfg)
			return nil
		})
	}
	members := make(chan error, 1)
	if cfg.Members != nil {
		launched := rec
		g.Go("members", func(ctx context.Context) error {
			members <- cfg.Members(ctx, launched)
			return nil
		})
	}
	if rec.Host && rec.AdminKubeconfig != "" {
		hostCfg := devinstance.HostConfig{Kubeconfig: rec.AdminKubeconfig, Manager: m, Own: devinstance.Prefixes(l.ID), Log: log}
		g.Go("devinstance-host", func(ctx context.Context) error {
			devinstance.RunHost(ctx, hostCfg)
			return nil
		})
	}
	var events <-chan hostsvc.UnitEvent
	watched := ""
	unwatch := context.CancelFunc(func() {})
	defer func() { unwatch() }()
	retry := time.NewTimer(0)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("core stopping", "generation", core.Generation)
			return g.Stop()
		case err := <-members:
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				err = errors.New("devcore: an in-process member returned")
			}
			log.Error("an in-process member ended; the core exits", "err", err.Error())
			return err
		case _, open := <-events:
			if !open {
				events, watched = nil, ""
			}
			for drained := events == nil; !drained; {
				select {
				case _, open := <-events:
					if !open {
						events, watched, drained = nil, "", true
					}
				default:
					drained = true
				}
			}
		case <-retry.C:
		}
		if ctx.Err() != nil {
			continue
		}
		cur, settled := check(ctx, m, bcfg, l, &core, cfg.OnTick, log)
		if prefix := unitPrefix(cur.Units); prefix != "" && prefix != watched {
			unwatch()
			events, watched = nil, ""
			wctx, cancel := context.WithCancel(ctx)
			if ch, err := m.Watch(wctx, prefix); err != nil {
				cancel()
				log.Error("watching the bootstrap units", "prefix", prefix, "err", err.Error())
				settled = false
			} else {
				events, watched, unwatch = ch, prefix, cancel
				retry.Reset(0)
				continue
			}
		}
		if !settled {
			retry.Reset(CheckInterval)
		}
	}
}

func check(ctx context.Context, m hostsvc.Manager, bcfg bootstrap.Config, l devinstance.Layout, core *devinstance.Core,
	onTick func(), log *slog.Logger) (devinstance.Record, bool) {
	if onTick != nil {
		onTick()
	}
	cur := devinstance.Record{}
	lk, ok := tryLockRecord(l.Lock)
	if !ok {
		log.Info("a dev verb holds the instance; this check waits for it")
		return cur, false
	}
	defer lk.Close()
	if err := fsx.ReadJSON(l.Record, &cur); err != nil {
		log.Error("re-reading the instance record", "err", err.Error())
		return cur, false
	}
	lost := false
	core.Verdicts = nil
	for _, u := range cur.Units {
		obs, restarted, err := bootstrap.Observe(ctx, m, u.Identity)
		if onTick != nil {
			onTick()
		}
		if err != nil {
			log.Error("check", "unit", u.Unit, "err", err.Error())
			continue
		}
		if restarted {
			lost = true
			log.Warn("unit restarted in place by devsup; re-identifying it", "unit", u.Unit, "detail", obs.Detail)
			core.Verdicts = append(core.Verdicts, fmt.Sprintf("%s=Restarted", u.Unit))
			continue
		}
		core.Verdicts = append(core.Verdicts, fmt.Sprintf("%s=%s %s", u.Unit, obs.Verdict, obs.Reason))
		if obs.Verdict == hostsvc.VerdictLost {
			lost = true
			log.Warn("unit lost", "unit", u.Unit, "reason", string(obs.Reason), "detail", obs.Detail)
		}
	}
	core.LastCheck = time.Now().UTC()
	if lost {
		if err := ensure(ctx, m, bcfg, l, &cur, core, log); err != nil {
			log.Error("re-placing the bootstrap units", "err", err.Error())
			return cur, false
		}
		return cur, true
	}
	fsx.WriteJSON(l.CoreRecord, *core, 0o600)
	return cur, true
}

func unitPrefix(units []devinstance.Unit) string {
	prefix := ""
	for i, u := range units {
		if i == 0 {
			prefix = strings.TrimSuffix(u.Unit, hostsvc.ServiceSuffix)
			continue
		}
		n := 0
		for n < len(prefix) && n < len(u.Unit) && prefix[n] == u.Unit[n] {
			n++
		}
		prefix = prefix[:n]
	}
	return prefix
}

func ensure(ctx context.Context, m hostsvc.Manager, bcfg bootstrap.Config, l devinstance.Layout, rec *devinstance.Record,
	core *devinstance.Core, log *slog.Logger) error {
	if err := bootstrap.Ensure(ctx, m, bcfg, rec); err != nil {
		return err
	}
	core.Adoptions = nil
	for _, u := range rec.Units {
		core.Adoptions = append(core.Adoptions, devinstance.Adoption{Role: u.Role, Unit: u.Unit,
			InvocationID: u.Identity.InvocationID, PID: u.Identity.PID, Adopted: u.Adopted})
	}
	core.AdoptedAt = time.Now().UTC()
	core.LastCheck = core.AdoptedAt
	if err := fsx.WriteJSON(l.Record, *rec, 0o600); err != nil {
		return err
	}
	if err := fsx.WriteJSON(l.CoreRecord, *core, 0o600); err != nil {
		return err
	}
	log.Info("core holds the instance", "generation", core.Generation, "adoptions", core.Adoptions)
	return nil
}

func rollDrifted(ctx context.Context, m hostsvc.Manager, bcfg bootstrap.Config, cfg Config, l devinstance.Layout, rec *devinstance.Record,
	core *devinstance.Core, log *slog.Logger) error {
	compat, err := versions.KcpCompatRecords()
	if err != nil {
		return err
	}
	d, rep, err := bootstrap.RollDrifted(ctx, m, bootstrap.InstallParams(*cfg.Application, l.StateDir),
		bootstrap.InstalledRoll{Compat: compat, ReadyTimeout: cfg.ReadyTimeout, Log: log})
	if err != nil {
		log.Error("the running kcp slot differs from what this binary renders and was not rolled; it keeps its old argv",
			"unit", d.Unit, "argvChanged", d.ArgvChanged, "exeChanged", d.ExeChanged, "refused", d.Refused, "err", err.Error())
		return nil
	}
	if rep == nil {
		return nil
	}
	log.Info("the kcp slot rolled to this binary's argv", "from", rep.Roll.From, "to", rep.Roll.To, "unit", d.Unit)
	return ensure(ctx, m, bcfg, l, rec, core, log)
}

func ComponentCoreConfig(ctx context.Context, cfg Config, rec devinstance.Record, log *slog.Logger) componentcore.Config {
	out := componentcore.Config{
		Kubeconfig:              rec.AdminKubeconfig,
		Workspaces:              componentcore.AppWorkspaces(rec.BaseWorkspace),
		StateDir:                rec.Layout.StateDir,
		Log:                     log,
		TLSCertFile:             cfg.TLSCertFile,
		TLSKeyFile:              cfg.TLSKeyFile,
		Handlers:                cfg.Handlers,
		ExposureIngressIdentity: componentcore.NewExposureIngressIdentity(rec.Layout.StateDir),
		SlotsFile:               rec.Layout.SlotsFile(),
		FrontControl:            rec.Layout.FrontControl(),
		InstallStateRoot:        install.StateRoot,
		NodeClaimAgent:          cfg.NodeClaimAgent,
	}
	if cfg.ComputeVM != nil {
		c := *cfg.ComputeVM
		c.Kubeconfig, c.Workspace = rec.AdminKubeconfig, cmp.Or(rec.BaseWorkspace, hostprocess.DefaultWorkspace)
		out.Compute = &c
	}
	if rec.AdminKubeconfig != "" {
		if n, err := marketnetwork.ReadNetwork(ctx, rec.AdminKubeconfig); err == nil && n.Status.Network != nil {
			out.PLCBaseURL = n.Status.Network.PLCDirectoryURL
			out.Relays = n.Status.Network.Relays()
		}
	}
	switch {
	case cfg.Application != nil && cfg.Application.Role == table.RoleBidder:
		out.CredentialClient = marketbidder.BidderVaultClient
		out.CredentialServer = marketbidder.BidderVaultServer
		out.OAuthScopes = atprotooauth.BidderOAuthScope
	case cfg.Application != nil && cfg.Application.Role == table.RoleGateway:
		out.CredentialClient = marketrequester.RequesterVaultClient
		out.CredentialServer = vaultlayout.RootServerName
		out.OAuthScopes = atprotooauth.RequesterOAuthScope
	case strings.Contains(rec.BaseWorkspace, ":gateway"), strings.Contains(rec.BaseWorkspace, ":ssh"):
		out.CredentialClient = marketrequester.RequesterVaultClient
		out.CredentialServer = vaultlayout.RootServerName
		out.OAuthScopes = atprotooauth.RequesterOAuthScope
	case strings.Contains(rec.BaseWorkspace, ":bidder"), strings.HasPrefix(rec.BaseWorkspace, "b-"), strings.HasPrefix(rec.Layout.ID, "b-"):
		out.CredentialClient = marketbidder.BidderVaultClient
		out.CredentialServer = marketbidder.BidderVaultServer
		out.OAuthScopes = atprotooauth.BidderOAuthScope
	case strings.HasPrefix(rec.BaseWorkspace, "r-"), strings.HasPrefix(rec.Layout.ID, "r-"):
		out.CredentialClient = marketrequester.RequesterVaultClient
		out.CredentialServer = vaultlayout.RootServerName
		out.OAuthScopes = atprotooauth.RequesterOAuthScope
	}
	if rec.Profile == bootstrap.ProfileQuick {
		out.InstallAccount = install.AccountBidder
	} else {
		out.InstallAccount = install.AccountGateway
	}
	if rec.Profile == bootstrap.ProfileMarketNetwork {
		out.Workspaces = componentcore.NetworkWorkspaces(rec.BaseWorkspace)
	}
	if rec.Profile == bootstrap.ProfileMarketNetwork && rec.AdminKubeconfig != "" {
		out.WorkspacesFunc = func(ctx context.Context) []string {
			n, err := marketnetwork.ReadNetwork(ctx, rec.AdminKubeconfig)
			if err != nil {
				return out.Workspaces
			}
			seen := map[string]bool{}
			var workspaces []string
			add := func(workspace string) {
				if workspace == "" || seen[workspace] {
					return
				}
				seen[workspace] = true
				workspaces = append(workspaces, workspace)
			}
			for _, workspace := range out.Workspaces {
				add(workspace)
			}
			for _, c := range n.Status.Components {
				add(c.Workspace)
			}
			return workspaces
		}
	}
	return out
}

var ErrNotReady = errors.New("devcore: the core did not record this generation in time")

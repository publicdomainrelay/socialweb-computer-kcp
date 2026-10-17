package devcore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/postgresctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/storemaint"
)

const HostProcessApplyTimeout = time.Minute

func Mirrors(bcfg bootstrap.Config, l devinstance.Layout) []hostprocess.Mirror {
	var rec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &rec); err != nil || rec.SharedKcp != "" {
		return nil
	}
	app, err := bcfg.App(&rec)
	if err != nil {
		return nil
	}
	tbl, err := table.Table(app)
	if err != nil {
		return nil
	}
	p := bcfg.Params(app)
	var out []hostprocess.Mirror
	for _, e := range tbl {
		if e.Kind == table.KindSlot && rec.Unit(e.Role) == nil {
			continue
		}
		spec, err := p.Spec(e, rec.KcpURL)
		if err != nil {
			continue
		}
		out = append(out, hostprocess.Mirror{Role: e.Role, Unit: e.Unit, Exec: spec.Exec, Slice: spec.Slice, After: e.After, Wants: e.Wants})
	}
	return out
}

func HostProcessConfig(bcfg bootstrap.Config, rec devinstance.Record, m hostsvc.Manager, log *slog.Logger) hostprocess.Config {
	l := bcfg.Layout
	ws := rec.BaseWorkspace
	if ws == "" {
		ws = hostprocess.DefaultWorkspace
	}
	return hostprocess.Config{Kubeconfig: rec.AdminKubeconfig, Workspace: ws, Manager: m, Prefix: l.Names().HostProcessPrefix(),
		Resolve: func(a v1alpha1.HostProcessAsset) (string, error) {
			if e, ok := rec.Executables[a.Name]; ok && e.Path != "" {
				return e.Path, nil
			}
			return "", fmt.Errorf("devcore: no executable %s in this dev instance", a.Name)
		},
		Mirrors:      func() []hostprocess.Mirror { return Mirrors(bcfg, l) },
		ApplyTimeout: HostProcessApplyTimeout, Selector: "!" + computevm.LabelComputeVM, Log: log}
}

func PostgresConfig(bcfg bootstrap.Config, rec devinstance.Record, log *slog.Logger) postgresctl.Config {
	l := bcfg.Layout
	ws := rec.BaseWorkspace
	if ws == "" {
		ws = hostprocess.DefaultWorkspace
	}
	root := rec.Executables["postgres"].Path
	return postgresctl.Config{Kubeconfig: rec.AdminKubeconfig, Workspace: ws, Prefix: l.Names().HostProcessPrefix(),
		Install: func() (pgunit.Install, error) {
			if root == "" {
				return pgunit.Install{}, fmt.Errorf("devcore: dev up was given no postgres-root")
			}
			return pgunit.FromRoot(root)
		},
		DataRoot: filepath.Join(l.StateDir, "postgres"), SocketRoot: l.RuntimeDir, LogDir: l.Logs, Log: log}
}

func AppConfig(bcfg bootstrap.Config, rec devinstance.Record, m hostsvc.Manager, log *slog.Logger) appctl.Config {
	l := bcfg.Layout
	return appctl.Config{Kubeconfig: rec.AdminKubeconfig, Workspace: rec.BaseWorkspace, Layout: l, Manager: m,
		Record: func() (devinstance.Record, error) {
			var r devinstance.Record
			return r, devinstance.ReadJSON(l.Record, &r)
		},
		ReadyTimeout: bcfg.ReadyTimeout, Log: log}
}

func OpenBaoServerConfig(bcfg bootstrap.Config, rec devinstance.Record, log *slog.Logger) openbaoctl.Config {
	hp := HostProcessConfig(bcfg, rec, nil, log)
	return openbaoctl.Config{Kubeconfig: hp.Kubeconfig, Workspace: hp.Workspace, StateDir: bcfg.Layout.StateDir, Prefix: hp.Prefix,
		Env: openbaoctl.OutboundEnv(os.Getenv), Resolve: hp.Resolve, Log: log}
}

func StoreMaintConfig(bcfg bootstrap.Config, rec devinstance.Record, log *slog.Logger) (storemaint.Config, error) {
	app, err := bcfg.App(&rec)
	if err != nil {
		return storemaint.Config{}, err
	}
	p := bcfg.Params(app)
	c := storemaint.Config{Kind: kcpstore.Kind(app.Store), PgSocketDir: p.PgSocketDir(), Log: log}
	for _, slot := range app.KineSlots() {
		c.SqliteDirs = append(c.SqliteDirs, p.KineDir(slot))
	}
	return c, nil
}

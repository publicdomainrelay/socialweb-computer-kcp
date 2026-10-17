package bootstrap

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	ProfileQuick         = "quick"
	ProfileProdlike      = "prodlike"
	ProfileMarketNetwork = "market-network"
	CoreStopTimeout      = 10 * time.Second
	CoreName             = "socialweb-computer"
	DropInName           = "socialweb-bootstrap.conf"
	InstallFrontAddr     = "127.0.0.1:0"
	PendingFrontURL      = "https://front.pending.invalid"
)

type Params struct {
	App          table.App
	Dir          string
	Run          string
	Logs         string
	Exe          string
	KcpPath      string
	KinePath     string
	PostgresRoot string
	FrontAddress string
	Owner        string
	InstanceDir  string
	ReadyTimeout time.Duration
}

type InstalledUnit struct {
	Entry table.Entry
	Unit  string
	Path  string
	Exec  string
	File  hostsvc.InstalledFile
}

func DevApps(profile string, l devinstance.Layout) ([]table.App, error) {
	switch profile {
	case ProfileQuick, ProfileMarketNetwork:
		return []table.App{table.Dev(l.Short, table.StoreSqlite)}, nil
	case ProfileProdlike:
		return []table.App{table.Dev(l.Short, table.StorePostgres)}, nil
	}
	return nil, fmt.Errorf("bootstrap: unknown profile %q: want quick, prodlike or market-network", profile)
}

func (c Config) Params(a table.App) Params {
	l := c.Layout
	p := Params{App: a, Exe: c.Exe, KcpPath: c.KcpPath, KinePath: c.KinePath, PostgresRoot: c.PostgresRoot,
		FrontAddress: c.frontAddress(), Owner: l.ID, InstanceDir: l.StateDir, ReadyTimeout: c.ReadyTimeout,
		Dir: l.KcpDir(), Run: l.RuntimeDir, Logs: l.Logs}
	if a.Key != l.Short {
		p.Dir = filepath.Join(l.StateDir, "apps", a.Key, "kcp")
		p.Run = filepath.Join(l.RuntimeDir, a.Key)
		p.Logs = filepath.Join(l.StateDir, "apps", a.Key, "logs")
		p.Owner = l.ID + "/" + a.Key
		p.InstanceDir = filepath.Join(l.StateDir, "apps", a.Key)
	}
	return p
}

func AppDir(a table.App, state string) string {
	if a.Role == table.RoleBidder {
		return filepath.Join(state, "handles", a.Key, "kcp")
	}
	return filepath.Join(state, "kcp")
}

func InstallParams(a table.App, state string) Params {
	dir := AppDir(a, state)
	bin := filepath.Join(dir, "bin")
	return Params{App: a, Dir: dir, Run: filepath.Join(dir, "run"), Exe: filepath.Join(state, "bin", "current", CoreName),
		KcpPath: filepath.Join(bin, "kcp"), KinePath: filepath.Join(bin, "kine"), PostgresRoot: filepath.Join(bin, pgunit.Asset),
		FrontAddress: InstallFrontAddr, Owner: a.Key, InstanceDir: state}
}

func (p Params) KineSocket(slot string) string { return filepath.Join(p.Run, "kine-"+slot+".sock") }

func (p Params) KineURL(slot string) string { return "unix://" + p.KineSocket(slot) }

func (p Params) KineDir(slot string) string { return filepath.Join(p.Dir, "kine-"+slot) }

func (p Params) PgSocketDir() string { return filepath.Join(p.Run, "pg") }

func (p Params) PgData() string { return filepath.Join(p.Dir, "pg", "data") }

func (p Params) SlotRoot(slot string) string { return filepath.Join(p.Dir, "slot-"+slot) }

func (p Params) FrontControl() string { return filepath.Join(p.Run, "front.sock") }

func (p Params) FrontState() string { return filepath.Join(p.Dir, "front.json") }

func (p Params) EnvFile(unit string) string { return filepath.Join(p.Dir, unit+hostsvc.EnvFileSuffix) }

func (p Params) BackupsDir() string { return filepath.Join(p.Dir, kcpstore.BackupsDir) }

func (p Params) SlotsFile() string { return filepath.Join(p.Dir, "kcp-slots.json") }

func (p Params) SharedDir() string { return filepath.Join(p.Dir, "shared") }

func (p Params) RollLock() string { return filepath.Join(p.Dir, "roll.lock") }

func (p Params) storeConfig(unit string, in pgunit.Install) kcpstore.PostgresConfig {
	return kcpstore.PostgresConfig{Unit: unit, Slice: p.App.Names().Slice(), Install: in, Data: p.PgData(), SocketDir: p.PgSocketDir()}
}

func (p Params) Postgres() (pgunit.Install, error) { return pgunit.FromRoot(p.PostgresRoot) }

func (p Params) shape() kcpserver.Shape {
	if p.App.Role == table.RoleBidder && !p.App.DevCore {
		return kcpserver.ShapeSingle
	}
	return kcpserver.ShapeBlueGreen
}

func (p Params) log(role string) hostsvc.Output {
	if p.Logs == "" {
		return hostsvc.Output{}
	}
	f := filepath.Join(p.Logs, role+".log")
	return hostsvc.Output{StdoutFile: f, StderrFile: f}
}

func (p Params) Spec(e table.Entry, frontURL string) (hostsvc.UnitSpec, error) {
	n := p.App.Names()
	var spec hostsvc.UnitSpec
	var err error
	switch e.Kind {
	case table.KindStore:
		var in pgunit.Install
		if in, err = pgunit.At(p.PostgresRoot); err == nil {
			spec, err = kcpstore.PostgresSpec(p.storeConfig(e.Unit, in))
		}
	case table.KindKine:
		kc := kcpstore.KineConfig{Unit: e.Unit, Slice: n.Slice(), Exec: p.KinePath, Dir: p.KineDir(e.Slot),
			Socket: p.KineSocket(e.Slot), PostgresSocketDir: p.PgSocketDir()}
		if p.App.Store == table.StorePostgres {
			spec, err = kcpstore.KinePostgresSpec(kc, n.Store())
		} else {
			spec, err = kcpstore.KineSqliteSpec(kc)
		}
	case table.KindFront:
		spec, err = kcpserver.FrontSpec(kcpserver.FrontConfig{Unit: e.Unit, Slice: n.Slice(), Exe: p.Exe, Owner: p.Owner,
			Address: p.FrontAddress, Control: p.FrontControl(), State: p.FrontState()})
	case table.KindSlot:
		spec, err = kcpserver.SlotSpec(kcpserver.SlotConfig{Unit: e.Unit, Slice: n.Slice(), Exec: p.KcpPath,
			Root: p.SlotRoot(e.Slot), EtcdServers: p.KineURL(p.App.KineOf(e.Slot)), FrontURL: frontURL, Shape: p.shape(),
			Wants: e.Wants})
		spec.After = e.After
	case table.KindCore:
		if !p.App.DevCore {
			return hostsvc.UnitSpec{}, errors.New("bootstrap: a production core is an installed unit of install's table, not a spec")
		}
		timeout := p.ReadyTimeout
		if timeout <= 0 {
			timeout = DefaultReadyTimeout
		}
		spec = hostsvc.UnitSpec{Name: e.Unit, Description: "dev core of " + p.Owner,
			Exec:    []string{p.Exe, "internal", "dev-core", "--instance-dir", p.InstanceDir, "--ready-timeout", timeout.String()},
			WorkDir: p.InstanceDir, Slice: n.CoreSlice(), Restart: hostsvc.RestartOnFailure, KillSignal: syscall.SIGTERM,
			StopTimeout: CoreStopTimeout, After: e.After, Wants: e.Wants, Delegate: true, OOMPolicy: "continue"}
	default:
		return hostsvc.UnitSpec{}, fmt.Errorf("bootstrap: unknown kind %q", e.Kind)
	}
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	spec.Output = p.log(e.Role)
	return spec, nil
}

func (c Config) CoreSpec(rec *devinstance.Record) (hostsvc.UnitSpec, error) {
	if len(rec.StandIns) > 0 {
		var units []string
		for _, s := range rec.StandIns {
			units = append(units, c.Layout.Names().StandIn(s.Role))
		}
		p := c.Params(table.Dev(c.Layout.Short, table.StoreSqlite))
		return p.Spec(table.Entry{Kind: table.KindCore, Role: table.RoleCore, Unit: c.Layout.CoreUnit(), After: units, Wants: units}, "")
	}
	if rec.SharedKcp != "" {
		p := c.Params(table.Dev(c.Layout.Short, table.StoreSqlite))
		return p.Spec(table.Entry{Kind: table.KindCore, Role: table.RoleCore, Unit: c.Layout.CoreUnit()}, "")
	}
	app, err := c.App(rec)
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	tbl, err := table.Table(app)
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	return c.Params(app).Spec(tbl[len(tbl)-1], "")
}

func ProcSpecs(p Params, frontURL string) ([]hostsvc.UnitSpec, error) {
	tbl, err := table.Table(p.App)
	if err != nil {
		return nil, err
	}
	var out []hostsvc.UnitSpec
	for _, e := range tbl {
		if e.Kind == table.KindCore && !p.App.DevCore {
			continue
		}
		s, err := p.Spec(e, frontURL)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func Installed(p Params, frontURL string) ([]InstalledUnit, error) {
	tbl, err := table.Table(p.App)
	if err != nil {
		return nil, err
	}
	var out []InstalledUnit
	for _, e := range tbl {
		if e.Kind == table.KindCore {
			out = append(out, InstalledUnit{Entry: e, Unit: e.Unit, Path: filepath.Join(e.Unit+".d", DropInName),
				File: hostsvc.InstalledFile{Unit: hostsvc.RenderDropIn(e.After, nil)}})
			continue
		}
		pending := e.Kind == table.KindSlot && frontURL == ""
		url := frontURL
		if pending {
			url = PendingFrontURL
		}
		spec, err := p.Spec(e, url)
		if err != nil {
			return nil, err
		}
		f, err := hostsvc.RenderInstalled(spec, hostsvc.InstallOptions{EnvFile: p.EnvFile(e.Unit)})
		if err != nil {
			return nil, err
		}
		if pending {
			f.Env = nil
		}
		out = append(out, InstalledUnit{Entry: e, Unit: e.Unit, Path: e.Unit, Exec: spec.Exec[0], File: f})
	}
	return out, nil
}

package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	PortPoll            = 20 * time.Millisecond
	ReadyPoll           = 200 * time.Millisecond
	FrontAddress        = "127.0.0.1:0"
	LeaseStaleAfter     = 6 * time.Second
	LeaseWatchWindow    = 12 * time.Second
	RestartSettle       = 3 * time.Second
	DefaultReadyTimeout = time.Minute
)

func CheckProfile(p string) error {
	if p == ProfileQuick || p == ProfileProdlike || p == ProfileMarketNetwork {
		return nil
	}
	return fmt.Errorf("bootstrap: unknown profile %q: want quick, prodlike or market-network", p)
}

func NeedsPostgres(profile string) bool { return profile == ProfileProdlike }

type Config struct {
	Layout       devinstance.Layout
	KcpPath      string
	KinePath     string
	PostgresRoot string
	Exe          string
	FrontAddress string
	ReadyTimeout time.Duration
	OnListen     func(name string, addr net.Addr)
	OnStep       func(step string)
	OnTick       func()
	Application  *table.App
	Retention    kcpstore.BackupRetention
	Log          *slog.Logger
	silent       map[string]bool
	portDeadline time.Time
}

func SlotRole(slot string) string { return table.SlotRole(slot) }

func (c Config) App(rec *devinstance.Record) (table.App, error) {
	if c.Application != nil {
		return *c.Application, nil
	}
	apps, err := DevApps(rec.Profile, c.Layout)
	if err != nil {
		return table.App{}, err
	}
	return apps[0], nil
}

func (c Config) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c Config) frontAddress() string {
	if c.FrontAddress != "" {
		return c.FrontAddress
	}
	return FrontAddress
}

func (c Config) onListen(name string, port int) {
	if c.OnListen != nil {
		c.OnListen(name, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	}
}

func (c Config) onStep(step string) {
	c.onTick()
	if c.OnStep != nil {
		c.OnStep(step)
	}
}

func (c Config) onTick() {
	if c.OnTick != nil {
		c.OnTick()
	}
}

func (p Params) slotSpec(slot, frontURL string) (hostsvc.UnitSpec, string, error) {
	spec, err := p.Spec(p.App.SlotEntry(slot), frontURL)
	return spec, p.SlotRoot(slot), err
}

func writeSlotEnv(p Params, spec hostsvc.UnitSpec) error {
	if _, err := os.Stat(p.EnvFile(p.App.Names().Front())); err != nil {
		return nil
	}
	f, err := hostsvc.RenderInstalled(spec, hostsvc.InstallOptions{EnvFile: p.EnvFile(spec.Name)})
	if err != nil {
		return err
	}
	if cur, err := os.ReadFile(f.EnvPath); err == nil && bytes.Equal(cur, f.Env) {
		return nil
	}
	return writePrivate(f.EnvPath, f.Env)
}

func waitUnixSocket(ctx context.Context, path string, progress ...func()) error {
	tick := func() {}
	if len(progress) > 0 && progress[0] != nil {
		tick = progress[0]
	}
	for {
		tick()
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bootstrap: %s never accepted a connection: %w (last: %v)", path, ctx.Err(), err)
		case <-time.After(PortPoll):
		}
	}
}

func initdb(ctx context.Context, p Params) error {
	in, err := p.Postgres()
	if err != nil {
		return err
	}
	_, err = in.Initdb(ctx, p.storeConfig("", in).Server())
	return err
}

func (p Params) storeBackup(r kcpstore.BackupRetention, log *slog.Logger) func(ctx context.Context) (string, error) {
	prune := func(dir, prefix, suffix, newest string) {
		removed, err := kcpstore.PruneBackups(dir, prefix, suffix, newest, r, time.Now())
		if err != nil {
			log.Warn("store backup prune incomplete", "dir", dir, "err", err)
		}
		if len(removed) > 0 {
			log.Info("store backups pruned", "dir", dir, "removed", removed, "keep", r.Keep, "maxAge", r.MaxAge.String())
		}
	}
	if p.App.Store != table.StorePostgres {
		return func(context.Context) (string, error) {
			kineDir := p.KineDir(p.App.KineOf(p.App.Slots[0]))
			out, err := kcpstore.BackupSqlite(kineDir, time.Now())
			if err != nil {
				return "", err
			}
			prune(filepath.Join(kineDir, kcpstore.BackupsDir), "", "", out)
			return out, nil
		}
	}
	return func(ctx context.Context) (string, error) {
		in, err := p.Postgres()
		if err != nil {
			return "", err
		}
		out, err := in.Dump(ctx, p.PgSocketDir(), kcpstore.PostgresUser, kcpstore.KineDatabase, p.BackupsDir(), time.Now())
		if err != nil {
			return "", err
		}
		prune(p.BackupsDir(), kcpstore.KineDatabase+"-", ".sql", out)
		return out, nil
	}
}

func waitPostgres(ctx context.Context, p Params, progress ...func()) error {
	if err := pgunit.WaitReady(ctx, p.PgSocketDir(), kcpstore.PostgresUser, progress...); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if _, err := pgunit.CreateDatabase(ctx, p.PgSocketDir(), kcpstore.PostgresUser, kcpstore.KineDatabase); err != nil {
		return fmt.Errorf("bootstrap: creating the kine database before any kine starts: %w", err)
	}
	return nil
}

func RSSKiB(pid int) int64 {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return n
		}
	}
	return 0
}

func Observe(ctx context.Context, m hostsvc.Manager, want hostsvc.Identity) (hostsvc.Observation, bool, error) {
	st, err := m.Inspect(ctx, want.Unit)
	deadline := time.Now().Add(RestartSettle)
	for err == nil && st.State == hostsvc.Activating && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return hostsvc.Observation{}, false, ctx.Err()
		case <-time.After(PortPoll):
		}
		st, err = m.Inspect(ctx, want.Unit)
	}
	if err == nil && st.State == hostsvc.Activating {
		return hostsvc.Observation{Verdict: hostsvc.VerdictUnknown, Status: st,
			Detail: fmt.Sprintf("%s is still activating after %s", want.Unit, RestartSettle)}, false, nil
	}
	if err == nil && st.State == hostsvc.Active && st.InvocationID != "" && want.InvocationID != "" &&
		st.InvocationID != want.InvocationID && st.MainPID > 0 {
		return hostsvc.Observation{Verdict: hostsvc.VerdictRunning, Status: st,
			Detail: fmt.Sprintf("%s was restarted in place by its manager (invocation %s, recorded %s)", want.Unit, st.InvocationID, want.InvocationID)}, true, nil
	}
	obs, err := m.Check(ctx, want)
	return obs, false, err
}

func identifyUnit(ctx context.Context, m hostsvc.Manager, cfg Config, role, name string, adopted bool, placedAt time.Time) (devinstance.Unit, error) {
	id, err := m.Identify(ctx, name, 0)
	if err != nil {
		return devinstance.Unit{}, err
	}
	if placedAt.IsZero() {
		placedAt = time.Now().UTC()
	}
	if cfg.silent[role] {
		return devinstance.Unit{Role: role, Unit: name, Identity: id, Adopted: adopted, PlacedAt: placedAt, PortAt: time.Now().UTC()}, nil
	}
	pctx, cancel := context.WithTimeout(ctx, cfg.ReadyTimeout)
	if !cfg.portDeadline.IsZero() {
		cancel()
		pctx, cancel = context.WithDeadline(ctx, cfg.portDeadline)
	}
	defer cancel()
	port, err := hostsvc.WaitListenPort(pctx, id.PID, PortPoll, cfg.onTick)
	if err != nil {
		return devinstance.Unit{}, fmt.Errorf("bootstrap: reading the bound port of %s (pid %d): %w", name, id.PID, err)
	}
	return devinstance.Unit{Role: role, Unit: name, Identity: id, Port: port, Endpoint: endpoint(role, port),
		Adopted: adopted, PlacedAt: placedAt, PortAt: time.Now().UTC()}, nil
}

func ensureUnit(ctx context.Context, m hostsvc.Manager, cfg Config, rec *devinstance.Record, role string, spec hostsvc.UnitSpec,
	beforeStart func() error) (devinstance.Unit, bool, error) {
	log := cfg.log().With("role", role, "unit", spec.Name)
	if prev := rec.Unit(role); prev != nil && prev.Identity.Unit == spec.Name {
		obs, restarted, err := Observe(ctx, m, prev.Identity)
		if err != nil {
			return devinstance.Unit{}, false, fmt.Errorf("bootstrap: checking %s: %w", spec.Name, err)
		}
		if restarted {
			u, err := identifyUnit(ctx, m, cfg, role, spec.Name, true, obs.Status.ActiveSince)
			if err != nil {
				return devinstance.Unit{}, false, err
			}
			log.Info("restarted in place by its manager; adopting the new invocation", "pid", u.Identity.PID,
				"invocation", u.Identity.InvocationID, "port", u.Port, "recordedPort", prev.Port)
			return u, false, nil
		}
		switch obs.Verdict {
		case hostsvc.VerdictRunning:
			u := *prev
			u.Adopted = true
			if port, err := hostsvc.ListenPort(u.Identity.PID); err == nil && port != u.Port && !cfg.silent[role] {
				log.Warn("an adopted unit listens on another port than recorded", "recorded", u.Port, "now", port)
				u.Port = port
				u.Endpoint = endpoint(role, port)
			}
			log.Info("adopted", "pid", u.Identity.PID, "invocation", u.Identity.InvocationID, "port", u.Port)
			return u, false, nil
		case hostsvc.VerdictUnknown:
			return devinstance.Unit{}, false, fmt.Errorf("bootstrap: %s is Unknown (%s); never re-created while the manager is unreachable", spec.Name, obs.Detail)
		}
		log.Info("recorded unit lost; placing it again", "reason", string(obs.Reason), "detail", obs.Detail)
	}
	if beforeStart != nil {
		if st, err := m.Inspect(ctx, spec.Name); err != nil || st.State != hostsvc.Active {
			if err := beforeStart(); err != nil {
				return devinstance.Unit{}, false, err
			}
		}
	}
	st, err := m.Start(ctx, spec)
	if err != nil {
		return devinstance.Unit{}, false, err
	}
	if st.Adopted && st.State != hostsvc.Active {
		log.Info("a loaded unit of this name is not active; resetting it", "state", string(st.State), "result", st.Result)
		if err := m.Stop(ctx, spec.Name); err != nil {
			return devinstance.Unit{}, false, err
		}
		if err := m.ResetFailed(ctx, spec.Name); err != nil {
			return devinstance.Unit{}, false, err
		}
		if beforeStart != nil {
			if err := beforeStart(); err != nil {
				return devinstance.Unit{}, false, err
			}
		}
		if st, err = m.Start(ctx, spec); err != nil {
			return devinstance.Unit{}, false, err
		}
		if st.Adopted && st.State != hostsvc.Active {
			log.Info("an installed unit of this name is loaded; starting it by name")
			if st, err = hostsvc.StartInstalled(ctx, m, spec.Name); err != nil {
				return devinstance.Unit{}, false, err
			}
		}
	}
	u, err := identifyUnit(ctx, m, cfg, role, spec.Name, st.Adopted, st.ActiveSince)
	if err != nil {
		return devinstance.Unit{}, false, err
	}
	log.Info("placed", "pid", u.Identity.PID, "invocation", u.Identity.InvocationID, "port", u.Port, "adopted", st.Adopted,
		"portAfter", u.PortAt.Sub(u.PlacedAt).String())
	return u, !st.Adopted, nil
}

func endpoint(role string, port int) string {
	return "https://127.0.0.1:" + strconv.Itoa(port)
}

func readSlots(l devinstance.Layout) kcpserver.Slots {
	s, err := kcpserver.ReadSlots(l.SlotsFile())
	if err != nil || s.Active == "" {
		return kcpserver.Slots{Shape: kcpserver.ShapeBlueGreen, Active: kcpserver.SlotA, Slots: map[string]kcpserver.SlotRecord{}}
	}
	return s
}

func stopRecordedSlots(ctx context.Context, m hostsvc.Manager, cfg Config, rec *devinstance.Record, why string) error {
	for _, slot := range kcpserver.SlotNames(kcpserver.ShapeBlueGreen) {
		prev := rec.Unit(SlotRole(slot))
		if prev == nil || prev.Identity.Unit == "" {
			continue
		}
		cfg.log().Info(why+"; the slot that dialed the old one is restarted", "slot", slot)
		if err := m.Stop(ctx, prev.Unit); err != nil {
			return err
		}
		m.ResetFailed(ctx, prev.Unit)
	}
	return nil
}

func HandoverStaleLease(ctx context.Context, admin *kcpserver.HTTPAdmin, log *slog.Logger) (string, error) {
	first, err := admin.Lease(ctx)
	if err != nil || !first.Found {
		return "absent", err
	}
	deadline := time.Now().Add(LeaseWatchWindow)
	for {
		l, err := admin.Lease(ctx)
		if err != nil {
			return "", err
		}
		if !l.Found || l.Holder != first.Holder || l.RenewTime.After(first.RenewTime) {
			return "live", nil
		}
		if time.Since(l.RenewTime) > LeaseStaleAfter {
			log.Info("the controller lease is held by a gone process; deleting it", "holder", l.Holder,
				"renewed", l.RenewTime.UTC().Format(time.RFC3339Nano))
			return "deleted", admin.DeleteLease(ctx)
		}
		if time.Now().After(deadline) {
			return "unchanged", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(ReadyPoll):
		}
	}
}

func Ensure(ctx context.Context, m hostsvc.Manager, cfg Config, rec *devinstance.Record) error {
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if len(rec.StandIns) > 0 {
		return ensureStandIns(ctx, m, cfg, rec)
	}
	if rec.SharedKcp != "" {
		rec.Ready, rec.Updated = true, time.Now().UTC()
		return nil
	}
	l := cfg.Layout
	log := cfg.log()
	app, err := cfg.App(rec)
	if err != nil {
		return err
	}
	p := cfg.Params(app)
	tbl, err := table.Table(app)
	if err != nil {
		return err
	}
	cfg.silent = map[string]bool{table.RolePostgres: true}
	for _, s := range app.KineSlots() {
		cfg.silent[table.KineRole(s)] = true
		if err := os.MkdirAll(p.KineDir(s), 0o700); err != nil {
			return err
		}
	}
	for _, d := range []string{p.Dir, p.Logs, p.Run} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	slots := readSlots(l)
	active := slots.Active
	if !slices.Contains(app.Slots, active) {
		active = app.Slots[0]
	}
	rctx, cancel := context.WithTimeout(ctx, cfg.ReadyTimeout)
	defer cancel()
	if d, ok := rctx.Deadline(); ok {
		cfg.portDeadline = d
	}
	var kine, front devinstance.Unit
	frontPlaced := false
	for _, e := range tbl {
		if e.Kind == table.KindSlot || e.Kind == table.KindCore {
			continue
		}
		spec, err := p.Spec(e, "")
		if err != nil {
			return err
		}
		var before func() error
		if e.Kind == table.KindStore {
			if err := os.MkdirAll(p.PgSocketDir(), 0o700); err != nil {
				return err
			}
			before = func() error { return initdb(ctx, p) }
		}
		cfg.onStep("placing " + e.Role)
		u, placed, err := ensureUnit(ctx, m, cfg, rec, e.Role, spec, before)
		if err != nil {
			return err
		}
		switch e.Kind {
		case table.KindStore:
			cfg.onStep("waiting for postgres")
			if err := waitPostgres(rctx, p, cfg.onTick); err != nil {
				return err
			}
			u.Endpoint = "unix://" + p.PgSocketDir()
		case table.KindKine:
			cfg.onStep("waiting for kine's unix socket")
			if err := waitUnixSocket(rctx, p.KineSocket(e.Slot), cfg.onTick); err != nil {
				return err
			}
			u.Endpoint = p.KineURL(e.Slot)
			if e.Slot == app.KineOf(active) {
				kine = u
				rec.KineURL = u.Endpoint
			}
		case table.KindFront:
			cfg.onListen(e.Role, u.Port)
			front, frontPlaced = u, placed
		}
		u.RSSKiB = RSSKiB(u.Identity.PID)
		rec.SetUnit(u)
	}
	frontURL := kcpserver.FrontURL(front.Port)
	if frontPlaced {
		if err := stopRecordedSlots(ctx, m, cfg, rec, "the front was placed again on a new port"); err != nil {
			return err
		}
	}
	role := SlotRole(active)
	slotSpec, root, err := p.slotSpec(active, frontURL)
	if err != nil {
		return err
	}
	if err := kcpserver.WriteAuditPolicy(root); err != nil {
		return err
	}
	if err := writeSlotEnv(p, slotSpec); err != nil {
		return err
	}
	seeded := false
	seed := func() error {
		if !kcpserver.HasShared(l.SharedDir()) {
			return nil
		}
		seeded = true
		return kcpserver.SeedSlot(l.SharedDir(), root)
	}
	prevInvocation := ""
	if prev := rec.Unit(role); prev != nil {
		prevInvocation = prev.Identity.InvocationID
	}
	cfg.onStep("placing the kcp slot")
	kcp, kcpPlaced, err := ensureUnit(ctx, m, cfg, rec, role, slotSpec, seed)
	if err != nil {
		return err
	}
	cfg.onListen(role, kcp.Port)
	cfg.onStep("waiting for kcp /readyz")
	var lastReady string
	reportReady := func(attempt int, last error) {
		cfg.onTick()
		msg := ""
		if last != nil {
			msg = last.Error()
		}
		if msg == lastReady {
			return
		}
		lastReady = msg
		log.Info("waiting for kcp /readyz", "attempt", attempt, "detail", msg)
	}
	var readyAt time.Time
	if kcpserver.HasShared(l.SharedDir()) {
		a, err := kcpserver.ReadSharedAdmin(l.SharedDir())
		if err != nil {
			return err
		}
		admin, err := kcpserver.NewHTTPAdmin(a, frontURL)
		if err != nil {
			return err
		}
		if err := kcpserver.WaitOwnPortReady(rctx, admin, kcp.Port, ReadyPoll, reportReady); err != nil {
			return fmt.Errorf("bootstrap: %s on 127.0.0.1:%d: %w", kcp.Unit, kcp.Port, err)
		}
		readyAt = time.Now().UTC()
	} else {
		notBefore := time.Time{}
		if kcpPlaced {
			notBefore = kcp.PlacedAt
		}
		readyAt, err = kcpserver.WaitReady(rctx, kcp.Port, filepath.Join(root, "admin.kubeconfig"), notBefore, ReadyPoll, reportReady)
		if err != nil {
			return fmt.Errorf("bootstrap: %s on 127.0.0.1:%d: %w", kcp.Unit, kcp.Port, err)
		}
		if err := kcpserver.SaveShared(root, l.SharedDir()); err != nil {
			return err
		}
		log.Info("shared root material saved", "dir", l.SharedDir(), "from", root)
	}
	if kcpPlaced || kcp.ReadyAt.IsZero() {
		kcp.ReadyAt = readyAt
		kcp.StartedTo = readyAt.Sub(kcp.PlacedAt).Round(time.Millisecond).String()
	}
	a, err := kcpserver.ReadSharedAdmin(l.SharedDir())
	if err != nil {
		return err
	}
	if err := kcpserver.WriteFrontKubeconfig(l.FrontKubeconfig(), frontURL, a); err != nil {
		return err
	}
	slots.Shape = p.shape()
	slots.Active = active
	slots.KineURL = kine.Endpoint
	slots.Front = kcpserver.FrontRecord{Unit: front.Unit, Address: "127.0.0.1:" + strconv.Itoa(front.Port), Port: front.Port,
		Control: l.FrontControl(), PID: front.Identity.PID}
	slots.Slots[active] = kcpserver.SlotRecord{Unit: kcp.Unit, Root: root, Port: kcp.Port, PID: kcp.Identity.PID,
		InvocationID: kcp.Identity.InvocationID, Exec: cfg.KcpPath, Kine: kine.Endpoint, StartedAt: kcp.PlacedAt, ReadyAt: kcp.ReadyAt}
	for _, other := range kcpserver.SlotNames(kcpserver.ShapeBlueGreen) {
		if other != active {
			delete(slots.Slots, other)
			rec.RemoveUnit(SlotRole(other))
		}
	}
	if err := kcpserver.WriteSlots(l.SlotsFile(), slots); err != nil {
		return err
	}
	fc := apiserverfront.Client{Path: l.FrontControl()}
	backend := "127.0.0.1:" + strconv.Itoa(kcp.Port)
	fst, err := fc.Set(ctx, backend)
	if err != nil {
		return fmt.Errorf("bootstrap: pointing the front at %s: %w", backend, err)
	}
	if admin, err := kcpserver.NewHTTPAdmin(a, frontURL); err == nil {
		if moved, err := admin.SyncRootShard(ctx); err != nil {
			log.Warn("pointing the root shard at the front", "front", frontURL, "err", err.Error())
		} else if moved {
			log.Info("the root shard advertises the front it runs behind now", "front", frontURL)
		}
		if n, err := admin.SyncEndpointSlices(ctx); err != nil {
			log.Warn("pointing the APIExport endpoint slices at the front", "front", frontURL, "err", err.Error())
		} else if n > 0 {
			log.Info("the APIExport endpoint slices name the front it runs behind now", "front", frontURL, "slices", n)
		}
	}
	if kcpPlaced || prevInvocation != kcp.Identity.InvocationID {
		admin, err := kcpserver.NewHTTPAdmin(a, frontURL)
		if err != nil {
			return err
		}
		verdict, err := HandoverStaleLease(ctx, admin, log)
		if err != nil {
			log.Warn("checking the controller lease after a slot start", "err", err.Error())
		}
		log.Info("controller lease after the slot start", "lease", verdict, "seeded", seeded)
	}
	kcp.RSSKiB = RSSKiB(kcp.Identity.PID)
	kine.RSSKiB = RSSKiB(kine.Identity.PID)
	front.RSSKiB = RSSKiB(front.Identity.PID)
	rec.SetUnit(kine)
	rec.SetUnit(front)
	rec.SetUnit(kcp)
	rec.KcpURL = frontURL
	rec.AdminKubeconfig = l.FrontKubeconfig()
	rec.Ready = true
	rec.Updated = time.Now().UTC()
	log.Info("kcp ready behind its front", "slot", active, "port", kcp.Port, "front", fst.Address, "backend", fst.Backend,
		"startToReady", kcp.StartedTo, "kcpRSSKiB", kcp.RSSKiB, "kineRSSKiB", kine.RSSKiB, "frontRSSKiB", front.RSSKiB)
	return nil
}

func Roll(ctx context.Context, m hostsvc.Manager, cfg Config, rec *devinstance.Record) (kcpserver.RollReport, error) {
	l := cfg.Layout
	slots, err := kcpserver.ReadSlots(l.SlotsFile())
	if err != nil {
		return kcpserver.RollReport{}, fmt.Errorf("bootstrap: no kcp-slots.json; dev up first: %w", err)
	}
	app, err := cfg.App(rec)
	if err != nil {
		return kcpserver.RollReport{}, err
	}
	p := cfg.Params(app)
	a, err := kcpserver.ReadSharedAdmin(l.SharedDir())
	if err != nil {
		return kcpserver.RollReport{}, err
	}
	frontURL := slots.Front.URL()
	admin, err := kcpserver.NewHTTPAdmin(a, frontURL)
	if err != nil {
		return kcpserver.RollReport{}, err
	}
	for _, slot := range kcpserver.SlotNames(kcpserver.ShapeBlueGreen) {
		if err := kcpserver.WriteAuditPolicy(l.SlotRoot(slot)); err != nil {
			return kcpserver.RollReport{}, err
		}
	}
	rep, after, err := kcpserver.Roll(ctx, m, apiserverfront.Client{Path: l.FrontControl()}, admin, kcpserver.RollConfig{
		SlotsFile: l.SlotsFile(), Shared: l.SharedDir(), MemberToken: kcpserver.MemberTokenPath(p.Dir),
		SpecFor: func(slot string) (hostsvc.UnitSpec, string, error) {
			return p.slotSpec(slot, frontURL)
		},
		KineFor:      func(slot string) string { return p.KineURL(app.KineOf(slot)) },
		Backup:       p.storeBackup(cfg.Retention, cfg.log()),
		ReadyTimeout: cfg.ReadyTimeout, OnListen: cfg.OnListen, Log: cfg.log(),
	})
	if after.Active == rep.To && rep.NewUnit != "" {
		if id, ierr := m.Identify(ctx, rep.NewUnit, 0); ierr == nil {
			rec.SetUnit(devinstance.Unit{Role: SlotRole(rep.To), Unit: rep.NewUnit, Identity: id, Port: rep.NewPort,
				Endpoint: endpoint(SlotRole(rep.To), rep.NewPort), PlacedAt: rep.StartedAt, PortAt: rep.PortAt,
				ReadyAt: rep.ReadyAt, StartedTo: rep.ReadyAt.Sub(rep.StartedAt).Round(time.Millisecond).String(),
				RSSKiB: RSSKiB(id.PID)})
		}
		if !rep.OldExitedAt.IsZero() {
			rec.RemoveUnit(SlotRole(rep.From))
		}
		rec.Updated = time.Now().UTC()
	}
	return rep, err
}

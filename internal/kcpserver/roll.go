package kcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultRollReadyTimeout = time.Minute
	DefaultLeaseTimeout     = 30 * time.Second
	DefaultProbeTimeout     = 10 * time.Second
	DefaultDrainTimeout     = 5 * time.Second
	DefaultRollPoll         = 200 * time.Millisecond
	PortPoll                = 20 * time.Millisecond
)

type FrontSwitch interface {
	Set(ctx context.Context, backend string) (apiserverfront.Status, error)
	Status(ctx context.Context) (apiserverfront.Status, error)
	Drain(ctx context.Context, backend string) (int, error)
}

type ShardAdmin interface {
	Readyz(ctx context.Context, port int) (int, error)
	Lease(ctx context.Context) (Lease, error)
	AwaitLease(ctx context.Context, timeout time.Duration, done func(Lease) bool) (Lease, error)
	DeleteLease(ctx context.Context) error
	ProbeWorkspace(ctx context.Context, name string) error
}

type RollConfig struct {
	SlotsFile    string
	Shared       string
	MemberToken  string
	SpecFor      func(slot string) (hostsvc.UnitSpec, string, error)
	KineFor      func(slot string) string
	Backup       func(ctx context.Context) (string, error)
	Start        func(ctx context.Context, slot string, spec hostsvc.UnitSpec) (hostsvc.UnitStatus, error)
	Retire       func(ctx context.Context, old, current string) error
	Abort        func(ctx context.Context) error
	Describe     func(slot string) (sha256, version string)
	Outage       bool
	ListenPort   func(ctx context.Context, pid int) (int, error)
	OnListen     func(name string, addr net.Addr)
	Poll         time.Duration
	ReadyTimeout time.Duration
	LeaseTimeout time.Duration
	ProbeTimeout time.Duration
	DrainTimeout time.Duration
	Log          *slog.Logger
}

type RollReport struct {
	From                 string    `json:"from"`
	To                   string    `json:"to"`
	PlannedOutage        bool      `json:"plannedOutage,omitempty"`
	OutageSeconds        float64   `json:"outageSeconds,omitempty"`
	OldUnit              string    `json:"oldUnit"`
	StoreBackup          string    `json:"storeBackup,omitempty"`
	BackupSeconds        float64   `json:"backupSeconds,omitempty"`
	NewUnit              string    `json:"newUnit"`
	OldPort              int       `json:"oldPort"`
	NewPort              int       `json:"newPort"`
	NewPID               int       `json:"newPID"`
	NewInvocationID      string    `json:"newInvocationID"`
	OldHolder            string    `json:"oldHolder"`
	NewHolder            string    `json:"newHolder"`
	StartedAt            time.Time `json:"startedAt"`
	PortAt               time.Time `json:"portAt"`
	ReadyAt              time.Time `json:"readyAt"`
	PersistedAt          time.Time `json:"persistedAt"`
	FlippedAt            time.Time `json:"flippedAt"`
	MemberTokenAt        time.Time `json:"memberTokenAt,omitzero"`
	SigtermAt            time.Time `json:"sigtermAt"`
	OldExitedAt          time.Time `json:"oldExitedAt"`
	LeaseDeletedAt       time.Time `json:"leaseDeletedAt"`
	LeaseHeldAt          time.Time `json:"leaseHeldAt"`
	ProbeReadyAt         time.Time `json:"probeReadyAt"`
	DoneAt               time.Time `json:"doneAt"`
	StartToReadySeconds  float64   `json:"startToReadySeconds"`
	OldExitSeconds       float64   `json:"oldExitSeconds"`
	LeaseHandoverSeconds float64   `json:"leaseHandoverSeconds"`
	ProbeSeconds         float64   `json:"probeSeconds"`
	TotalSeconds         float64   `json:"totalSeconds"`
	OpenOnOldAtExit      int       `json:"openOnOldAtExit"`
	Drained              int       `json:"drained"`
}

func (c *RollConfig) defaults() {
	c.Poll = opt.Positive(c.Poll, DefaultRollPoll)
	c.ReadyTimeout = opt.Positive(c.ReadyTimeout, DefaultRollReadyTimeout)
	c.LeaseTimeout = opt.Positive(c.LeaseTimeout, DefaultLeaseTimeout)
	c.ProbeTimeout = opt.Positive(c.ProbeTimeout, DefaultProbeTimeout)
	c.DrainTimeout = opt.Positive(c.DrainTimeout, DefaultDrainTimeout)
	if c.ListenPort == nil {
		c.ListenPort = func(ctx context.Context, pid int) (int, error) { return hostsvc.WaitListenPort(ctx, pid, PortPoll) }
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
}

func backendOf(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func WaitOwnPortReady(ctx context.Context, admin ShardAdmin, port int, every time.Duration,
	progress ...func(attempt int, last error)) error {
	every = opt.Positive(every, DefaultRollPoll)
	var last error
	for attempt := 1; ; attempt++ {
		code, err := admin.Readyz(ctx, port)
		if err == nil && code == http.StatusOK {
			return nil
		}
		last = fmt.Errorf("readyz on 127.0.0.1:%d: %d %v", port, code, err)
		report(progress, attempt, last)
		select {
		case <-ctx.Done():
			return fmt.Errorf("kcpserver: not ready: %v (%w)", last, ctx.Err())
		case <-time.After(every):
		}
	}
}

func WaitNewHolder(ctx context.Context, admin ShardAdmin, old string) (Lease, error) {
	l, err := admin.AwaitLease(ctx, 0, func(l Lease) bool { return l.Found && l.Holder != "" && l.Holder != old })
	if err != nil {
		return Lease{}, fmt.Errorf("kcpserver: no new holder of %s/%s: holder %q (%w)", ControllerLeaseNamespace, ControllerLeaseName, l.Holder, err)
	}
	return l, nil
}

func awaitDrained(ctx context.Context, front FrontSwitch, backend string, timeout, every time.Duration) {
	every = opt.Positive(every, DefaultRollPoll)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		fst, err := front.Status(ctx)
		if err != nil || fst.Open[backend] == 0 || !clock.Sleep(ctx, every) {
			return
		}
	}
}

func Roll(ctx context.Context, m hostsvc.Manager, front FrontSwitch, admin ShardAdmin, cfg RollConfig) (RollReport, Slots, error) {
	cfg.defaults()
	log := cfg.Log.With("component", "kcp-roll")
	s, err := ReadSlots(cfg.SlotsFile)
	if err != nil {
		return RollReport{}, Slots{}, fmt.Errorf("kcpserver: reading %s: %w", cfg.SlotsFile, err)
	}
	from := s.Active
	old, ok := s.Slots[from]
	if !ok || old.Unit == "" {
		return RollReport{}, s, fmt.Errorf("kcpserver: %s names active slot %q, which it does not record", cfg.SlotsFile, from)
	}
	single := s.Shape != ShapeBlueGreen
	to := Other(from)
	if single {
		to = from
	}
	outage := cfg.Outage || single
	start := cfg.Start
	if start == nil {
		start = func(ctx context.Context, _ string, spec hostsvc.UnitSpec) (hostsvc.UnitStatus, error) {
			return m.Start(ctx, spec)
		}
	}
	rep := RollReport{From: from, To: to, PlannedOutage: outage, OldUnit: old.Unit, OldPort: old.Port, StartedAt: time.Now().UTC()}
	if l, err := admin.Lease(ctx); err == nil {
		rep.OldHolder = l.Holder
	}
	if cfg.Backup != nil {
		path, err := cfg.Backup(ctx)
		if err != nil {
			return rep, s, fmt.Errorf("kcpserver: the store backup before the roll: %w", err)
		}
		rep.StoreBackup, rep.BackupSeconds = path, clock.SecondsBetween(rep.StartedAt, time.Now().UTC())
		log.Info("store backed up before the roll", "path", path, "seconds", rep.BackupSeconds)
	}
	spec, root, err := cfg.SpecFor(to)
	if err != nil {
		return rep, s, err
	}
	rep.NewUnit = spec.Name
	if err := SeedSlot(cfg.Shared, root); err != nil {
		return rep, s, err
	}
	stopOld := func(ctx context.Context) error {
		rep.SigtermAt = time.Now().UTC()
		if err := m.Stop(ctx, old.Unit); err != nil {
			return fmt.Errorf("kcpserver: stopping the old slot %s: %w", old.Unit, err)
		}
		if ost, err := m.Inspect(ctx, old.Unit); err == nil && (ost.State == hostsvc.Active || ost.State == hostsvc.Deactivating) {
			return fmt.Errorf("kcpserver: the old slot %s is still %s after its stop", old.Unit, ost.State)
		}
		m.ResetFailed(ctx, old.Unit)
		rep.OldExitedAt = time.Now().UTC()
		rep.OldExitSeconds = clock.SecondsBetween(rep.SigtermAt, rep.OldExitedAt)
		return nil
	}
	if outage {
		log.Info("planned outage: stopping the old slot before the new one starts", "slot", from, "unit", old.Unit)
		if err := stopOld(ctx); err != nil {
			return rep, s, err
		}
	}
	m.Stop(ctx, spec.Name)
	m.ResetFailed(ctx, spec.Name)
	restoreOld := func(ctx context.Context) error {
		ospec, oroot, err := cfg.SpecFor(from)
		if err != nil {
			return err
		}
		st, err := start(ctx, from, ospec)
		if err != nil {
			return err
		}
		port, err := cfg.ListenPort(ctx, st.MainPID)
		if err != nil {
			return err
		}
		if err := WaitOwnPortReady(ctx, admin, port, cfg.Poll); err != nil {
			return err
		}
		rec := old
		rec.Port, rec.PID, rec.InvocationID = port, st.MainPID, st.InvocationID
		s.Slots[from], s.Active = rec, from
		if err := WriteSlots(cfg.SlotsFile, s); err != nil {
			return err
		}
		if _, err := front.Set(ctx, backendOf(port)); err != nil {
			return err
		}
		if cfg.MemberToken != "" {
			RefreshMemberTokenIfPresent(oroot, cfg.MemberToken)
		}
		log.Warn("planned outage aborted; the old slot runs again behind the front", "slot", from, "port", port)
		return nil
	}
	fail := func(err error) (RollReport, Slots, error) {
		log.Error("roll aborted before the flip; stopping the new slot", "slot", to, "err", err.Error())
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SlotStopTimeout+cfg.ReadyTimeout)
		defer cancel()
		m.Stop(sctx, spec.Name)
		m.ResetFailed(sctx, spec.Name)
		if cfg.Abort != nil {
			if aerr := cfg.Abort(sctx); aerr != nil {
				err = errors.Join(err, aerr)
			}
		}
		if outage {
			if rerr := restoreOld(sctx); rerr != nil {
				err = errors.Join(err, fmt.Errorf("kcpserver: restarting the old slot %s: %w", old.Unit, rerr))
			}
		}
		return rep, s, err
	}
	st, err := start(ctx, to, spec)
	if err != nil {
		return fail(err)
	}
	if st.Adopted {
		return fail(fmt.Errorf("kcpserver: %s was already running after its stop; refusing to adopt a stale slot", spec.Name))
	}
	rep.NewPID, rep.NewInvocationID = st.MainPID, st.InvocationID
	pctx, cancel := context.WithTimeout(ctx, cfg.ReadyTimeout)
	defer cancel()
	port, err := cfg.ListenPort(pctx, st.MainPID)
	if err != nil {
		return fail(fmt.Errorf("kcpserver: reading the bound port of %s (pid %d): %w", spec.Name, st.MainPID, err))
	}
	rep.NewPort, rep.PortAt = port, time.Now().UTC()
	if cfg.OnListen != nil {
		cfg.OnListen("kcp-"+to, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	}
	log.Info("new slot listening", "slot", to, "port", port, "pid", st.MainPID)
	if err := WaitOwnPortReady(pctx, admin, port, cfg.Poll); err != nil {
		return fail(err)
	}
	rep.ReadyAt = time.Now().UTC()
	rep.StartToReadySeconds = clock.SecondsBetween(rep.StartedAt, rep.ReadyAt)
	prev := s
	prev.Slots = map[string]SlotRecord{}
	maps.Copy(prev.Slots, s.Slots)
	next := s
	next.Slots = map[string]SlotRecord{}
	maps.Copy(next.Slots, s.Slots)
	next.Slots[to] = SlotRecord{Unit: spec.Name, Root: root, Port: port, PID: st.MainPID, InvocationID: st.InvocationID,
		Exec: spec.Exec[0], StartedAt: rep.StartedAt, ReadyAt: rep.ReadyAt}
	if cfg.KineFor != nil {
		rec := next.Slots[to]
		rec.Kine = cfg.KineFor(to)
		next.Slots[to] = rec
		next.KineURL = rec.Kine
	}
	if cfg.Describe != nil {
		rec := next.Slots[to]
		rec.SHA256, rec.Version = cfg.Describe(to)
		next.Slots[to] = rec
	}
	next.Active = to
	if err := WriteSlots(cfg.SlotsFile, next); err != nil {
		return fail(err)
	}
	s = next
	rep.PersistedAt = time.Now().UTC()
	if _, err := front.Set(ctx, backendOf(port)); err != nil {
		WriteSlots(cfg.SlotsFile, prev)
		s = prev
		return fail(fmt.Errorf("kcpserver: flipping the front: %w", err))
	}
	rep.FlippedAt = time.Now().UTC()
	if cfg.MemberToken != "" {
		if changed, err := RefreshMemberTokenIfPresent(root, cfg.MemberToken); err != nil {
			log.Warn("the members' kcp-admin token was not rewritten at the flip; the instance's own check follows within seconds", "slot", to, "tokenFile", cfg.MemberToken, "err", err.Error())
		} else if changed {
			rep.MemberTokenAt = time.Now().UTC()
		}
	}
	log.Info("front flipped", "from", backendOf(old.Port), "to", backendOf(port), "memberTokenAt", rep.MemberTokenAt)
	if outage {
		rep.OutageSeconds = clock.SecondsBetween(rep.SigtermAt, rep.FlippedAt)
	} else {
		if err := stopOld(ctx); err != nil {
			return rep, s, err
		}
		if fst, err := front.Status(ctx); err == nil {
			rep.OpenOnOldAtExit = fst.Open[backendOf(old.Port)]
		}
	}
	if single {
		rep.LeaseHeldAt = rep.FlippedAt
	} else {
		if err := admin.DeleteLease(ctx); err != nil {
			return rep, s, err
		}
		rep.LeaseDeletedAt = time.Now().UTC()
		lctx, lcancel := context.WithTimeout(ctx, cfg.LeaseTimeout)
		defer lcancel()
		l, err := WaitNewHolder(lctx, admin, rep.OldHolder)
		if err != nil {
			return rep, s, err
		}
		rep.NewHolder, rep.LeaseHeldAt = l.Holder, time.Now().UTC()
		rep.LeaseHandoverSeconds = clock.SecondsBetween(rep.OldExitedAt, rep.LeaseHeldAt)
		log.Info("controller lease handed over", "from", rep.OldHolder, "to", rep.NewHolder, "seconds", rep.LeaseHandoverSeconds)
	}
	probe := "roll-probe-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	prctx, prcancel := context.WithTimeout(ctx, cfg.ProbeTimeout)
	defer prcancel()
	if err := admin.ProbeWorkspace(prctx, probe); err != nil {
		return rep, s, fmt.Errorf("kcpserver: the probe workspace did not reconcile after the roll: %w", err)
	}
	rep.ProbeReadyAt = time.Now().UTC()
	rep.ProbeSeconds = clock.SecondsBetween(rep.LeaseHeldAt, rep.ProbeReadyAt)
	if !outage {
		awaitDrained(ctx, front, backendOf(old.Port), cfg.DrainTimeout, cfg.Poll)
	}
	if n, err := front.Drain(ctx, backendOf(old.Port)); err == nil {
		rep.Drained = n
	} else if !errors.Is(err, apiserverfront.ErrCurrentBackend) {
		log.Warn("draining the old backend", "err", err.Error())
	}
	if to != from {
		delete(s.Slots, from)
		if cfg.Retire != nil {
			if err := cfg.Retire(ctx, from, to); err != nil {
				return rep, s, fmt.Errorf("kcpserver: retiring slot %s: %w", from, err)
			}
		}
	}
	if err := WriteSlots(cfg.SlotsFile, s); err != nil {
		return rep, s, err
	}
	rep.DoneAt = time.Now().UTC()
	rep.TotalSeconds = clock.SecondsBetween(rep.StartedAt, rep.DoneAt)
	return rep, s, nil
}

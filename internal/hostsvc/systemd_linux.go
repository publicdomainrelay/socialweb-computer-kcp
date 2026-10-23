//go:build linux

package hostsvc

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const (
	errUnitExists           = "org.freedesktop.systemd1.UnitExists"
	errNoSuchUnit           = "org.freedesktop.systemd1.NoSuchUnit"
	errNoUnitForPID         = "org.freedesktop.systemd1.NoUnitForPID"
	errNoSuchProcess        = "org.freedesktop.systemd1.NoSuchProcess"
	errUnixProcessIDUnknown = "org.freedesktop.DBus.Error.UnixProcessIdUnknown"
)

var _ host.Units = (*systemd)(nil)

type systemd struct {
	dial    func(ctx context.Context) (bus, error)
	procs   procs
	timeout time.Duration
	prefix  string

	mu sync.Mutex
	b  bus
}

func openSystemd(ctx context.Context, cfg Config) (Manager, error) {
	path := cfg.SystemdPrivateSocket
	if path == "" {
		path = PrivateSocketPath()
	}
	m, err := OpenPrivate(ctx, path)
	if err != nil {
		return nil, err
	}
	m.(*systemd).prefix = cfg.Prefix()
	return m, nil
}

func OpenPrivate(ctx context.Context, path string) (Manager, error) {
	m := newManager(dialPrivate(path), linuxProcs{}, DefaultCallTimeout)
	if err := m.checkVersion(ctx); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func newManager(dial func(ctx context.Context) (bus, error), p procs, timeout time.Duration) *systemd {
	return &systemd{dial: dial, procs: p, timeout: timeout, prefix: NamePrefix}
}

func (m *systemd) Capabilities() Capabilities {
	return Capabilities{Backend: BackendSystemdUser, EnforcesLimits: true, OOMKillReported: true, FDStore: true}
}

func (m *systemd) owns(name string) error {
	if !strings.HasPrefix(name, m.prefix) {
		return fmt.Errorf("hostsvc: unit %s is outside this manager's prefix %s", name, m.prefix)
	}
	return nil
}

func (m *systemd) managerID(ctx context.Context) (string, error) {
	var started uint64
	err := m.do(ctx, func(ctx context.Context, b bus) (err error) {
		started, err = b.ManagerStarted(ctx)
		return err
	})
	if err != nil {
		return "", err
	}
	boot, err := m.procs.BootID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("systemd-user:%s:%d", boot, started), nil
}

func (m *systemd) StopSlice(ctx context.Context, slice string) error {
	if err := ValidateSlice(slice); err != nil {
		return err
	}
	ctx, cancel := withDeadline(ctx, m.timeout)
	defer cancel()
	ch := make(chan string, 1)
	err := m.do(ctx, func(ctx context.Context, b bus) error { return b.StopUnit(ctx, slice, ch) })
	if dbusErrorName(err) == errNoSuchUnit {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hostsvc: stopping %s: %w", slice, err)
	}
	result, err := waitJob(ctx, ch)
	if err != nil {
		return fmt.Errorf("hostsvc: stopping %s: %w", slice, err)
	}
	if result != "done" {
		return fmt.Errorf("hostsvc: the stop job of %s ended %q", slice, result)
	}
	return nil
}

const unitPathPrefix = "/org/freedesktop/systemd1/unit/"

func unitKey(st UnitStatus) string {
	return string(st.State) + "|" + st.SubState + "|" + st.InvocationID + "|" + st.Result
}

func unescapeUnitPath(p dbus.ObjectPath) string {
	s, ok := strings.CutPrefix(string(p), unitPathPrefix)
	if !ok || strings.Contains(s, "/") {
		return ""
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '_' && i+2 < len(s) {
			if b, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				out = append(out, b[0])
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

func signalUnit(sig *dbus.Signal) string {
	switch sig.Name {
	case managerIfc + ".UnitNew", managerIfc + ".UnitRemoved":
		if len(sig.Body) > 0 {
			name, _ := sig.Body[0].(string)
			return name
		}
	case managerIfc + ".JobRemoved":
		if len(sig.Body) > 2 {
			name, _ := sig.Body[2].(string)
			return name
		}
	case "org.freedesktop.DBus.Properties.PropertiesChanged":
		if len(sig.Body) > 0 {
			if ifc, _ := sig.Body[0].(string); ifc == systemdDest+".Unit" || ifc == systemdDest+".Service" {
				return unescapeUnitPath(sig.Path)
			}
		}
	}
	return ""
}

func (m *systemd) current(ctx context.Context, name string) (UnitStatus, error) {
	var units []sd.UnitStatus
	err := m.do(ctx, func(ctx context.Context, b bus) (err error) {
		units, err = b.ListUnitsByPatterns(ctx, []string{name})
		return err
	})
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: looking up %s: %w", name, err)
	}
	for _, u := range units {
		if u.Name == name && u.LoadState != "not-found" {
			return m.Inspect(ctx, name)
		}
	}
	return UnitStatus{Name: name, State: NotFound}, nil
}

func (m *systemd) Watch(ctx context.Context, prefix string) (<-chan UnitEvent, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	dctx, cancel := withDeadline(ctx, m.timeout)
	b, err := m.dial(dctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	sigs := make(chan *dbus.Signal, 256)
	sctx, cancel := withDeadline(ctx, m.timeout)
	err = b.Subscribe(sctx, sigs)
	cancel()
	if err != nil {
		b.Close()
		if unreachable(err) {
			return nil, fmt.Errorf("%w: subscribing to unit signals: %w", ErrUnreachable, err)
		}
		return nil, fmt.Errorf("hostsvc: subscribing to unit signals: %w", err)
	}
	first, err := m.List(ctx, prefix)
	if err != nil {
		b.Close()
		return nil, err
	}
	seen := map[string]string{}
	for _, st := range first {
		seen[st.Name] = unitKey(st)
	}
	ch := make(chan UnitEvent, 64)
	stop := context.AfterFunc(ctx, b.Close)
	go func() {
		defer close(ch)
		defer stop()
		defer b.Close()
		for sig := range sigs {
			name := signalUnit(sig)
			if !strings.HasPrefix(name, prefix) || ValidateName(name) != nil {
				continue
			}
			st, err := m.current(ctx, name)
			if errors.Is(err, ErrUnreachable) || ctx.Err() != nil {
				return
			}
			if err != nil {
				continue
			}
			if st.State == NotFound {
				if _, ok := seen[name]; !ok {
					continue
				}
				delete(seen, name)
			} else {
				k := unitKey(st)
				if seen[name] == k {
					continue
				}
				seen[name] = k
			}
			select {
			case ch <- UnitEvent{Name: name, Status: st}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func withDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func (m *systemd) conn(ctx context.Context) (bus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.b != nil {
		return m.b, nil
	}
	b, err := m.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	m.b = b
	return b, nil
}

func (m *systemd) drop(b bus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.b == b {
		m.b = nil
	}
	b.Close()
}

func (m *systemd) do(ctx context.Context, f func(ctx context.Context, b bus) error) error {
	ctx, cancel := withDeadline(ctx, m.timeout)
	defer cancel()
	for retried := false; ; retried = true {
		b, err := m.conn(ctx)
		if err != nil {
			return err
		}
		err = f(ctx, b)
		if err == nil || !unreachable(err) {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		m.drop(b)
		if retried {
			return fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
	}
}

func unreachable(err error) bool {
	if err == nil {
		return false
	}
	var de dbus.Error
	if errors.As(err, &de) {
		return false
	}
	var dl dialError
	if errors.As(err, &dl) {
		return true
	}
	for _, target := range []error{context.DeadlineExceeded, context.Canceled, dbus.ErrClosed, net.ErrClosed,
		io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE, syscall.ENOTCONN, syscall.ECONNREFUSED} {
		if errors.Is(err, target) {
			return true
		}
	}
	var op *net.OpError
	return errors.As(err, &op)
}

func dbusErrorName(err error) string {
	var de dbus.Error
	if errors.As(err, &de) {
		return de.Name
	}
	return ""
}

type envFile struct {
	Path          string
	IgnoreMissing bool
}

func unitProperties(spec UnitSpec) []sd.Property {
	desc := spec.Description
	desc = cmp.Or(desc, spec.Name)
	slice := spec.Slice
	slice = cmp.Or(slice, DefaultSlice)
	oom := spec.OOMPolicy
	oom = cmp.Or(oom, "stop")
	str := func(name, v string) sd.Property { return sd.Property{Name: name, Value: dbus.MakeVariant(v)} }
	u64 := func(name string, v uint64) sd.Property { return sd.Property{Name: name, Value: dbus.MakeVariant(v)} }
	props := []sd.Property{
		sd.PropDescription(desc),
		sd.PropExecStart(spec.Exec, false),
		str("Type", "exec"),
		str("CollectMode", "inactive"),
		str("KillMode", "control-group"),
		str("OOMPolicy", oom),
		sd.PropSlice(slice),
	}
	restart := spec.Restart
	restart = cmp.Or(restart, RestartNo)
	props = append(props, str("Restart", string(restart)))
	if spec.KillSignal != 0 {
		props = append(props, sd.Property{Name: "KillSignal", Value: dbus.MakeVariant(int32(spec.KillSignal))})
	}
	if spec.StopTimeout > 0 {
		props = append(props, u64("TimeoutStopUSec", uint64(spec.StopTimeout/time.Microsecond)))
	}
	if spec.Limits.MemoryMax > 0 {
		props = append(props, u64("MemoryMax", spec.Limits.MemoryMax), u64("MemorySwapMax", 0))
	}
	if spec.Delegate {
		props = append(props, sd.Property{Name: "Delegate", Value: dbus.MakeVariant(true)})
	}
	if spec.Limits.TasksMax > 0 {
		props = append(props, u64("TasksMax", spec.Limits.TasksMax))
	}
	if spec.Limits.CPUWeight > 0 {
		props = append(props, u64("CPUWeight", spec.Limits.CPUWeight))
	}
	if spec.Limits.CPUQuotaPercent > 0 {
		props = append(props, u64("CPUQuotaPerSecUSec", uint64(spec.Limits.CPUQuotaPercent)*10_000))
	}
	if len(spec.Env) > 0 {
		props = append(props, sd.Property{Name: "Environment", Value: dbus.MakeVariant(spec.Env)})
	}
	if len(spec.EnvFiles) > 0 {
		files := make([]envFile, 0, len(spec.EnvFiles))
		for _, f := range spec.EnvFiles {
			files = append(files, envFile{Path: f})
		}
		props = append(props, sd.Property{Name: "EnvironmentFiles", Value: dbus.MakeVariant(files)})
	}
	if spec.WorkDir != "" {
		props = append(props, str("WorkingDirectory", spec.WorkDir))
	}
	if spec.Output.StdoutFile != "" {
		props = append(props, str("StandardOutputFileToAppend", spec.Output.StdoutFile))
	}
	if spec.Output.StderrFile != "" {
		props = append(props, str("StandardErrorFileToAppend", spec.Output.StderrFile))
	}
	if len(spec.After) > 0 {
		props = append(props, sd.PropAfter(spec.After...))
	}
	if len(spec.Wants) > 0 {
		props = append(props, sd.PropWants(spec.Wants...))
	}
	if spec.FDStore > 0 {
		props = append(props, sd.Property{Name: "FileDescriptorStoreMax", Value: dbus.MakeVariant(uint32(spec.FDStore))},
			str("NotifyAccess", "main"))
	}
	return props
}

func (m *systemd) checkVersion(ctx context.Context) error {
	var v string
	err := m.do(ctx, func(ctx context.Context, b bus) (err error) {
		v, err = b.Version(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("hostsvc: reading the manager version: %w", err)
	}
	n, err := strs.LeadingInt(v)
	if err != nil {
		return fmt.Errorf("%w: the manager reports version %q", ErrSystemdTooOld, v)
	}
	if n < MinSystemdVersion {
		return fmt.Errorf("%w: the manager reports %q", ErrSystemdTooOld, v)
	}
	return nil
}

func waitJob(ctx context.Context, ch <-chan string) (string, error) {
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return "", fmt.Errorf("%w: waiting for the job: %w", ErrUnreachable, ctx.Err())
	}
}

func (m *systemd) Start(ctx context.Context, spec UnitSpec) (UnitStatus, error) {
	if err := ValidateSpec(spec); err != nil {
		return UnitStatus{}, err
	}
	if err := m.owns(spec.Name); err != nil {
		return UnitStatus{}, err
	}
	if err := RefuseUnsupported(m.Capabilities(), spec); err != nil {
		return UnitStatus{}, err
	}
	ctx, cancel := withDeadline(ctx, m.timeout)
	defer cancel()
	props := unitProperties(spec)
	ch := make(chan string, 1)
	err := m.do(ctx, func(ctx context.Context, b bus) error {
		return b.StartTransientUnit(ctx, spec.Name, props, ch)
	})
	if dbusErrorName(err) == errUnitExists {
		st, err := m.Inspect(ctx, spec.Name)
		st.Adopted = true
		return st, err
	}
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: starting %s: %w", spec.Name, err)
	}
	result, err := waitJob(ctx, ch)
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: starting %s: %w", spec.Name, err)
	}
	st, err := m.Inspect(ctx, spec.Name)
	if err != nil {
		return st, err
	}
	if result != "done" {
		return st, fmt.Errorf("hostsvc: the start job of %s ended %q (state %s, result %s, exec main code %d status %d)",
			spec.Name, result, st.State, st.Result, st.ExecMainCode, st.ExecMainStatus)
	}
	return st, nil
}

type unitStarter interface {
	StartUnit(ctx context.Context, name string, ch chan<- string) error
}

type bootFiles interface {
	EnableUnitFiles(ctx context.Context, names []string) error
	DisableUnitFiles(ctx context.Context, names []string) error
}

func (m *systemd) SetBoot(ctx context.Context, enable, disable []string) error {
	for _, n := range append(slices.Clone(enable), disable...) {
		if err := ValidateName(n); err != nil {
			return err
		}
		if err := m.owns(n); err != nil {
			return err
		}
	}
	ctx, cancel := withDeadline(ctx, m.timeout)
	defer cancel()
	return m.do(ctx, func(ctx context.Context, b bus) error {
		s, ok := b.(bootFiles)
		if !ok {
			return fmt.Errorf("%w: this bus enables no unit file", ErrUnsupported)
		}
		if len(disable) > 0 {
			if err := s.DisableUnitFiles(ctx, disable); err != nil {
				return fmt.Errorf("hostsvc: disabling %v: %w", disable, err)
			}
		}
		if len(enable) > 0 {
			if err := s.EnableUnitFiles(ctx, enable); err != nil {
				return fmt.Errorf("hostsvc: enabling %v: %w", enable, err)
			}
		}
		return nil
	})
}

func (m *systemd) StartInstalled(ctx context.Context, name string) (UnitStatus, error) {
	if err := ValidateName(name); err != nil {
		return UnitStatus{}, err
	}
	if err := m.owns(name); err != nil {
		return UnitStatus{}, err
	}
	ctx, cancel := withDeadline(ctx, m.timeout)
	defer cancel()
	if st, err := m.Inspect(ctx, name); err == nil && st.State == Active {
		st.Adopted = true
		return st, nil
	}
	ch := make(chan string, 1)
	err := m.do(ctx, func(ctx context.Context, b bus) error {
		s, ok := b.(unitStarter)
		if !ok {
			return fmt.Errorf("%w: this bus starts no installed unit", ErrUnsupported)
		}
		return s.StartUnit(ctx, name, ch)
	})
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: starting the installed unit %s: %w", name, err)
	}
	result, err := waitJob(ctx, ch)
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: starting the installed unit %s: %w", name, err)
	}
	st, err := m.Inspect(ctx, name)
	if err != nil {
		return st, err
	}
	if result != "done" {
		return st, fmt.Errorf("hostsvc: the start job of the installed unit %s ended %q (state %s, result %s)", name, result, st.State, st.Result)
	}
	return st, nil
}

func (m *systemd) Inspect(ctx context.Context, name string) (UnitStatus, error) {
	if err := ValidateName(name); err != nil {
		return UnitStatus{}, err
	}
	var up, sp map[string]any
	err := m.do(ctx, func(ctx context.Context, b bus) (err error) {
		if up, err = b.UnitProperties(ctx, name); err != nil {
			return err
		}
		sp, err = b.ServiceProperties(ctx, name)
		return err
	})
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: inspecting %s: %w", name, err)
	}
	return statusFrom(name, up, sp), nil
}

func statusFrom(name string, up, sp map[string]any) UnitStatus {
	st := UnitStatus{Name: name, Backend: BackendSystemdUser}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	if str(up, "LoadState") == "not-found" {
		st.State = NotFound
		return st
	}
	st.State = ActiveState(str(up, "ActiveState"))
	st.SubState = str(up, "SubState")
	st.Description = str(up, "Description")
	st.Result = str(sp, "Result")
	if inv, ok := up["InvocationID"].([]byte); ok {
		st.InvocationID = hex.EncodeToString(inv)
	}
	if ts, ok := up["ActiveEnterTimestamp"].(uint64); ok && ts > 0 {
		st.ActiveSince = time.UnixMicro(int64(ts)).UTC()
	}
	if pid, ok := sp["MainPID"].(uint32); ok {
		st.MainPID = int(pid)
	}
	st.ExecMainCode, _ = sp["ExecMainCode"].(int32)
	st.ExecMainStatus, _ = sp["ExecMainStatus"].(int32)
	return st
}

func (m *systemd) Stop(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	ctx, cancel := withDeadline(ctx, m.timeout)
	defer cancel()
	ch := make(chan string, 1)
	err := m.do(ctx, func(ctx context.Context, b bus) error { return b.StopUnit(ctx, name, ch) })
	if dbusErrorName(err) == errNoSuchUnit {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hostsvc: stopping %s: %w", name, err)
	}
	result, err := waitJob(ctx, ch)
	if err != nil {
		return fmt.Errorf("hostsvc: stopping %s: %w", name, err)
	}
	if result != "done" {
		return fmt.Errorf("hostsvc: the stop job of %s ended %q", name, result)
	}
	return nil
}

func (m *systemd) ResetFailed(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	err := m.do(ctx, func(ctx context.Context, b bus) error { return b.ResetFailedUnit(ctx, name) })
	if dbusErrorName(err) == errNoSuchUnit {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hostsvc: resetting %s: %w", name, err)
	}
	return nil
}

func (m *systemd) List(ctx context.Context, prefix string) ([]UnitStatus, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	var units []sd.UnitStatus
	err := m.do(ctx, func(ctx context.Context, b bus) (err error) {
		units, err = b.ListUnitsByPatterns(ctx, []string{prefix + "*"})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("hostsvc: listing %s*: %w", prefix, err)
	}
	var out []UnitStatus
	for _, u := range units {
		if !strings.HasPrefix(u.Name, prefix) || !strings.HasSuffix(u.Name, ServiceSuffix) || u.LoadState == "not-found" {
			continue
		}
		st, err := m.Inspect(ctx, u.Name)
		if err != nil {
			return nil, err
		}
		if st.State != NotFound {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *systemd) unitByPIDFD(ctx context.Context, fd int) (string, string, error) {
	var unit, inv string
	err := m.do(ctx, func(ctx context.Context, b bus) (err error) {
		unit, inv, err = b.UnitByPIDFD(ctx, fd)
		return err
	})
	return unit, inv, err
}

func (m *systemd) Identify(ctx context.Context, name string, pid int) (Identity, error) {
	st, err := m.Inspect(ctx, name)
	if err != nil {
		return Identity{}, err
	}
	if st.State != Active {
		return Identity{}, fmt.Errorf("hostsvc: %s is %s, not active, and has no identity to record", name, st.State)
	}
	pid = cmp.Or(pid, st.MainPID)
	boot, err := m.procs.BootID()
	if err != nil {
		return Identity{}, err
	}
	p, err := m.procs.Pin(pid)
	if err != nil {
		return Identity{}, fmt.Errorf("hostsvc: pinning pid %d of %s: %w", pid, name, err)
	}
	defer p.Close()
	unit, inv, err := m.unitByPIDFD(ctx, p.FD())
	if err != nil {
		return Identity{}, fmt.Errorf("hostsvc: GetUnitByPIDFD for pid %d of %s: %w", pid, name, err)
	}
	if unit != name || inv != st.InvocationID {
		return Identity{}, fmt.Errorf("hostsvc: pid %d belongs to %s invocation %s, not %s invocation %s",
			pid, unit, inv, name, st.InvocationID)
	}
	mgr, err := m.managerID(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("hostsvc: reading the manager identity: %w", err)
	}
	return Identity{Unit: name, InvocationID: inv, PID: pid, StartTicks: p.StartTicks(), BootID: boot,
		Backend: BackendSystemdUser, Manager: mgr}, nil
}

func (m *systemd) Check(ctx context.Context, want Identity) (Observation, error) {
	if err := ValidateName(want.Unit); err != nil {
		return Observation{}, err
	}
	boot, err := m.procs.BootID()
	if err != nil {
		return Observation{Detail: err.Error()}, err
	}
	if boot != want.BootID {
		return Observation{Verdict: VerdictLost, Reason: LostHostRebooted,
			Detail: fmt.Sprintf("boot id %s, recorded %s", boot, want.BootID)}, nil
	}
	if obs, foreign := ForeignIdentity(m.Capabilities(), want); foreign {
		return obs, nil
	}
	if want.Manager != "" {
		mgr, err := m.managerID(ctx)
		if errors.Is(err, ErrUnreachable) {
			return Observation{Verdict: VerdictUnknown, Detail: err.Error()}, nil
		}
		if err != nil {
			return Observation{Detail: err.Error()}, err
		}
		if mgr != want.Manager {
			return Observation{Verdict: VerdictLost, Reason: LostUnitGone,
				Detail: fmt.Sprintf("the user manager restarted (%s, recorded %s): every unit it held is gone", mgr, want.Manager)}, nil
		}
	}
	st, err := m.Inspect(ctx, want.Unit)
	if errors.Is(err, ErrUnreachable) {
		return Observation{Verdict: VerdictUnknown, Detail: err.Error()}, nil
	}
	if err != nil {
		return Observation{Detail: err.Error()}, err
	}
	lost := func(r LostReason, format string, args ...any) (Observation, error) {
		return Observation{Verdict: VerdictLost, Reason: r, Detail: fmt.Sprintf(format, args...), Status: st}, nil
	}
	switch st.State {
	case NotFound:
		return lost(LostUnitGone, "%s is not loaded", want.Unit)
	case Failed:
		if st.Result == "oom-kill" {
			return lost(LostOOMKilled, "%s failed: result %s", want.Unit, st.Result)
		}
		return lost(LostUnitFailed, "%s failed: result %s, exec main code %d status %d",
			want.Unit, st.Result, st.ExecMainCode, st.ExecMainStatus)
	case Inactive:
		return lost(LostUnitExited, "%s is inactive: result %s", want.Unit, st.Result)
	}
	if st.InvocationID != want.InvocationID {
		return m.mismatch(ctx, st, "%s runs invocation %s, recorded %s", want.Unit, st.InvocationID, want.InvocationID)
	}
	p, err := m.procs.Pin(want.PID)
	if errors.Is(err, errProcessGone) {
		return m.mismatch(ctx, st, "recorded pid %d of %s: %v", want.PID, want.Unit, err)
	}
	if err != nil {
		return Observation{Detail: err.Error(), Status: st}, err
	}
	defer p.Close()
	if p.StartTicks() != want.StartTicks {
		return m.mismatch(ctx, st, "pid %d started at tick %d, recorded %d: the pid was reused",
			want.PID, p.StartTicks(), want.StartTicks)
	}
	unit, inv, err := m.unitByPIDFD(ctx, p.FD())
	switch {
	case errors.Is(err, ErrUnreachable):
		return Observation{Verdict: VerdictUnknown, Detail: err.Error(), Status: st}, nil
	case dbusErrorName(err) == errUnixProcessIDUnknown || dbusErrorName(err) == errNoUnitForPID ||
		dbusErrorName(err) == errNoSuchProcess:
		return m.mismatch(ctx, st, "GetUnitByPIDFD for pid %d: %v", want.PID, err)
	case err != nil:
		return Observation{Detail: err.Error(), Status: st}, fmt.Errorf("hostsvc: GetUnitByPIDFD for pid %d: %w", want.PID, err)
	}
	if unit != want.Unit || inv != want.InvocationID {
		return m.mismatch(ctx, st, "pid %d belongs to %s invocation %s, recorded %s invocation %s",
			want.PID, unit, inv, want.Unit, want.InvocationID)
	}
	if st.State != Active {
		return Observation{Verdict: VerdictUnknown, Detail: fmt.Sprintf("%s is %s", want.Unit, st.State), Status: st}, nil
	}
	return Observation{Verdict: VerdictRunning, Status: st}, nil
}

func (m *systemd) mismatch(ctx context.Context, st UnitStatus, format string, args ...any) (Observation, error) {
	obs := Observation{Verdict: VerdictLost, Reason: LostIdentityMismatch, Detail: fmt.Sprintf(format, args...), Status: st}
	if err := m.Stop(ctx, st.Name); err != nil {
		return obs, fmt.Errorf("hostsvc: %s does not match its record and stopping it failed: %w", st.Name, err)
	}
	obs.Stopped = true
	return obs, nil
}

func (m *systemd) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.b != nil {
		m.b.Close()
		m.b = nil
	}
	return nil
}

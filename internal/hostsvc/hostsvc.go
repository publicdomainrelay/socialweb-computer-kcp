package hostsvc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/systemdunit"
)

const NamePrefix = "socialweb-"

const ServiceSuffix = ".service"

const DefaultSlice = "socialweb.slice"

const MinSystemdVersion = 253

const DefaultCallTimeout = 10 * time.Second

var ErrUnsupported = errors.New("hostsvc: no service manager backend on this OS")

var ErrUnreachable = errors.New("hostsvc: service manager unreachable")

var ErrNoBackend = errors.New("hostsvc: no placement backend chosen: set the host-backend option (systemd-user | proc, env SOCIALWEB_HOST_BACKEND); there is no default and nothing is probed")

var ErrSystemdTooOld = fmt.Errorf("hostsvc: systemd older than %d (GetUnitByPIDFD is required)", MinSystemdVersion)

type Backend string

const (
	BackendSystemdUser Backend = "systemd-user"
	BackendProc        Backend = "proc"
)

func ParseBackend(s string) (Backend, error) {
	switch Backend(s) {
	case BackendSystemdUser, BackendProc:
		return Backend(s), nil
	case "":
		return "", ErrNoBackend
	}
	return "", fmt.Errorf("hostsvc: unknown placement backend %q: want %s or %s", s, BackendSystemdUser, BackendProc)
}

type Capabilities struct {
	Backend         Backend
	EnforcesLimits  bool
	OOMKillReported bool
	FDStore         bool
	PrivateHostname bool
	Journal         bool
}

type Config struct {
	Backend              Backend
	UnitPrefix           string
	SystemdPrivateSocket string
	ProcSocket           string
	InstalledDir         string
}

func (c Config) Prefix() string {
	if c.UnitPrefix == "" {
		return NamePrefix
	}
	return c.UnitPrefix
}

func (c Config) Validate() error {
	if c.Backend == "" {
		return ErrNoBackend
	}
	if _, err := ParseBackend(string(c.Backend)); err != nil {
		return err
	}
	if !strings.HasPrefix(c.Prefix(), NamePrefix) {
		return fmt.Errorf("hostsvc: unit prefix %q must start with %s", c.UnitPrefix, NamePrefix)
	}
	if c.Backend == BackendProc && c.ProcSocket == "" {
		return fmt.Errorf("hostsvc: the proc backend needs the devsup socket path")
	}
	return nil
}

type Restart string

const (
	RestartNo        Restart = Restart(systemdunit.RestartNo)
	RestartOnFailure Restart = Restart(systemdunit.RestartOnFailure)
	RestartAlways    Restart = Restart(systemdunit.RestartAlways)
)

type UnitEvent struct {
	Name   string
	Status UnitStatus
}

type ActiveState string

const (
	Active       ActiveState = "active"
	Activating   ActiveState = "activating"
	Deactivating ActiveState = "deactivating"
	Reloading    ActiveState = "reloading"
	Inactive     ActiveState = "inactive"
	Failed       ActiveState = "failed"
	NotFound     ActiveState = "not-found"
)

type Output struct {
	StdoutFile string
	StderrFile string
}

type Limits struct {
	MemoryMax       uint64
	CPUWeight       uint64
	CPUQuotaPercent int
	TasksMax        uint64
}

type UnitSpec struct {
	Name            string
	Description     string
	Exec            []string
	Env             []string
	EnvFiles        []string
	WorkDir         string
	Output          Output
	Limits          Limits
	OOMPolicy       string
	KillSignal      syscall.Signal
	StopTimeout     time.Duration
	After           []string
	Wants           []string
	Slice           string
	Restart         Restart
	PrivateHostname string
	FDStore         int
	Delegate        bool
}

type UnitStatus struct {
	Name           string
	State          ActiveState
	SubState       string
	Result         string
	MainPID        int
	InvocationID   string
	ExecMainCode   int32
	ExecMainStatus int32
	ActiveSince    time.Time
	Adopted        bool
	Description    string
	Backend        Backend
}

type Verdict int

const (
	VerdictUnknown Verdict = iota
	VerdictRunning
	VerdictLost
)

func (v Verdict) String() string {
	switch v {
	case VerdictRunning:
		return "Running"
	case VerdictLost:
		return "Lost"
	}
	return "Unknown"
}

type LostReason string

const (
	LostHostRebooted     LostReason = "HostRebooted"
	LostUnitGone         LostReason = "UnitGone"
	LostUnitExited       LostReason = "UnitExited"
	LostUnitFailed       LostReason = "UnitFailed"
	LostOOMKilled        LostReason = "OOMKilled"
	LostIdentityMismatch LostReason = "IdentityMismatch"
)

type Identity struct {
	Unit         string
	InvocationID string
	PID          int
	StartTicks   uint64
	BootID       string
	Backend      Backend
	Manager      string
}

type Observation struct {
	Verdict Verdict
	Reason  LostReason
	Detail  string
	Status  UnitStatus
	Stopped bool
}

type Manager interface {
	Start(ctx context.Context, spec UnitSpec) (UnitStatus, error)
	Inspect(ctx context.Context, name string) (UnitStatus, error)
	Stop(ctx context.Context, name string) error
	ResetFailed(ctx context.Context, name string) error
	List(ctx context.Context, prefix string) ([]UnitStatus, error)
	Identify(ctx context.Context, name string, pid int) (Identity, error)
	Check(ctx context.Context, want Identity) (Observation, error)
	StopSlice(ctx context.Context, slice string) error
	Watch(ctx context.Context, prefix string) (<-chan UnitEvent, error)
	Capabilities() Capabilities
	Close() error
}

func Live(st UnitStatus) bool {
	return st.State == Active || st.State == Activating || st.State == Deactivating
}

func Await(ctx context.Context, m Manager, unit string, done func(UnitStatus) bool) (UnitStatus, error) {
	if err := ValidateName(unit); err != nil {
		return UnitStatus{}, err
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := m.Watch(wctx, unit)
	if err != nil {
		return UnitStatus{}, err
	}
	st, err := m.Inspect(ctx, unit)
	if err != nil {
		return st, err
	}
	for !done(st) {
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return st, err
				}
				return st, fmt.Errorf("%w: the watch of %s ended", ErrUnreachable, unit)
			}
			if ev.Name == unit {
				st = ev.Status
			}
		}
	}
	return st, nil
}

func ValidateName(name string) error {
	if !strings.HasPrefix(name, NamePrefix) || !strings.HasSuffix(name, ServiceSuffix) {
		return fmt.Errorf("hostsvc: unit %q is not ours: every unit this package starts is named %s<kind>-<owner>-<name>%s",
			name, NamePrefix, ServiceSuffix)
	}
	if strings.ContainsAny(name, "*?[]/\\ ") {
		return fmt.Errorf("hostsvc: unit %q carries a glob or path character", name)
	}
	return nil
}

func ValidatePrefix(prefix string) error {
	if !strings.HasPrefix(prefix, NamePrefix) || len(prefix) <= len(NamePrefix) {
		return fmt.Errorf("hostsvc: list prefix %q must start with %s and name a kind", prefix, NamePrefix)
	}
	if strings.ContainsAny(prefix, "*?[]/\\ ") {
		return fmt.Errorf("hostsvc: list prefix %q carries a glob or path character", prefix)
	}
	return nil
}

func ValidateSpec(spec UnitSpec) error {
	if err := ValidateName(spec.Name); err != nil {
		return err
	}
	if len(spec.Exec) == 0 || !strings.HasPrefix(spec.Exec[0], "/") {
		return fmt.Errorf("hostsvc: unit %s needs an absolute executable path, got %q", spec.Name, spec.Exec)
	}
	if spec.Slice == "app.slice" || strings.HasPrefix(spec.Slice, "app-") {
		return fmt.Errorf("hostsvc: unit %s may not run in %s: systemd-oomd kills by memory pressure there",
			spec.Name, spec.Slice)
	}
	if spec.Slice != "" && !strings.HasSuffix(spec.Slice, ".slice") {
		return fmt.Errorf("hostsvc: unit %s names slice %q, which is not a .slice", spec.Name, spec.Slice)
	}
	switch spec.Restart {
	case "", RestartNo, RestartOnFailure, RestartAlways:
	default:
		return fmt.Errorf("hostsvc: unit %s asks for Restart=%q: want no, on-failure or always", spec.Name, spec.Restart)
	}
	if spec.FDStore < 0 {
		return fmt.Errorf("hostsvc: unit %s asks for a negative fd store", spec.Name)
	}
	if spec.Limits.CPUQuotaPercent < 0 {
		return fmt.Errorf("hostsvc: unit %s asks for a negative CPU quota", spec.Name)
	}
	return nil
}

func ValidateSlice(slice string) error {
	if !strings.HasPrefix(slice, "socialweb") || !strings.HasSuffix(slice, ".slice") {
		return fmt.Errorf("hostsvc: slice %q is not ours: every slice this package stops is named socialweb[-<key>].slice", slice)
	}
	if strings.ContainsAny(slice, "*?[]/\\ ") {
		return fmt.Errorf("hostsvc: slice %q carries a glob or path character", slice)
	}
	return nil
}

func SliceContains(slice, unitSlice string) bool {
	unitSlice = cmp.Or(unitSlice, DefaultSlice)
	if unitSlice == slice {
		return true
	}
	return strings.HasPrefix(unitSlice, strings.TrimSuffix(slice, ".slice")+"-")
}

func RefuseUnsupported(c Capabilities, spec UnitSpec) error {
	if spec.FDStore > 0 && !c.FDStore {
		return fmt.Errorf("%w: unit %s asks for an fd store, which the %s backend does not offer in this build",
			ErrUnsupported, spec.Name, c.Backend)
	}
	if spec.PrivateHostname != "" && !c.PrivateHostname {
		return fmt.Errorf("%w: unit %s asks for a private hostname, which the %s backend does not offer in this build",
			ErrUnsupported, spec.Name, c.Backend)
	}
	return nil
}

func ForeignIdentity(c Capabilities, want Identity) (Observation, bool) {
	if want.Backend == "" || want.Backend == c.Backend {
		return Observation{}, false
	}
	return Observation{Verdict: VerdictLost, Reason: LostIdentityMismatch,
		Detail: fmt.Sprintf("%s was placed by the %s backend and this manager is %s: never adopted across backends",
			want.Unit, want.Backend, c.Backend)}, true
}

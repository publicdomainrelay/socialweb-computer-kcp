package hostsvc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
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

type (
	Backend      = host.Backend
	Capabilities = host.Capabilities
	Restart      = host.Restart
	UnitEvent    = host.UnitEvent
	ActiveState  = host.ActiveState
	Output       = host.Output
	Limits       = host.Limits
	UnitSpec     = host.UnitSpec
	UnitStatus   = host.UnitStatus
	Verdict      = host.Verdict
	LostReason   = host.LostReason
	Identity     = host.Identity
	Observation  = host.Observation
	Manager      = host.Units
)

const (
	BackendSystemdUser = host.BackendSystemdUser
	BackendProc        = host.BackendProc
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

const (
	RestartNo        = host.RestartNo
	RestartOnFailure = host.RestartOnFailure
	RestartAlways    = host.RestartAlways
)

const (
	Active       = host.Active
	Activating   = host.Activating
	Deactivating = host.Deactivating
	Reloading    = host.Reloading
	Inactive     = host.Inactive
	Failed       = host.Failed
	NotFound     = host.NotFound
)

const (
	VerdictUnknown = host.VerdictUnknown
	VerdictRunning = host.VerdictRunning
	VerdictLost    = host.VerdictLost
)

const (
	LostHostRebooted     = host.LostHostRebooted
	LostUnitGone         = host.LostUnitGone
	LostUnitExited       = host.LostUnitExited
	LostUnitFailed       = host.LostUnitFailed
	LostOOMKilled        = host.LostOOMKilled
	LostIdentityMismatch = host.LostIdentityMismatch
)

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

var _ host.Clock = SystemClock{}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

var _ host.Processes = Processes{}

type Processes struct{}

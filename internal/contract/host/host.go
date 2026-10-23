package host

import (
	"context"
	"syscall"
	"time"
)

type Backend string

const (
	BackendSystemdUser Backend = "systemd-user"
	BackendProc        Backend = "proc"
)

type Capabilities struct {
	Backend         Backend
	EnforcesLimits  bool
	OOMKillReported bool
	FDStore         bool
	PrivateHostname bool
	Journal         bool
}

type Restart string

const (
	RestartNo        Restart = "no"
	RestartOnFailure Restart = "on-failure"
	RestartAlways    Restart = "always"
)

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

type UnitEvent struct {
	Name   string
	Status UnitStatus
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

type Units interface {
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

type Processes interface {
	Wait(ctx context.Context, pid int, startTicks uint64) error
}

type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

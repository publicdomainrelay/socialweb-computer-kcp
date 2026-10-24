package hostprocess

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

type OpKind string

const (
	OpStart OpKind = "start"
	OpStop  OpKind = "stop"
)

type Op struct {
	Kind OpKind
	Spec hostsvc.UnitSpec
	Unit string
}

type Observed struct {
	Object     v1alpha1.HostProcess
	Backend    hostsvc.Backend
	Unit       hostsvc.UnitStatus
	Inspect    error
	Check      *hostsvc.Observation
	Exec       string
	Prefix     string
	Deleting   bool
	Restarted  bool
	StartedKey string
}

func RestartedInPlace(o v1alpha1.HostProcess, st hostsvc.UnitStatus) bool {
	rt := o.Status.Runtime
	return rt != nil && rt.InvocationID != "" && o.Spec.RestartPolicy != "" && o.Spec.RestartPolicy != v1alpha1.HostProcessRestartNever &&
		(st.State == hostsvc.Activating || st.State == hostsvc.Active && st.MainPID > 0 && st.InvocationID != "" && st.InvocationID != rt.InvocationID)
}

type Result struct {
	Phase      v1alpha1.HostProcessPhase
	LostReason v1alpha1.HostProcessLostReason
	Message    string
	Ops        []Op
	Identify   bool
	Unknown    bool
}

var ErrNotOurs = errors.New("hostprocess: a supervisor places only the units under its own prefix")

func restart(p v1alpha1.HostProcessRestartPolicy) hostsvc.Restart {
	switch p {
	case v1alpha1.HostProcessRestartAlways:
		return hostsvc.RestartAlways
	case v1alpha1.HostProcessRestartOnFailure:
		return hostsvc.RestartOnFailure
	}
	return hostsvc.RestartNo
}

func signal(s string) syscall.Signal {
	switch s {
	case "SIGINT", "INT":
		return syscall.SIGINT
	case "SIGKILL", "KILL":
		return syscall.SIGKILL
	case "SIGQUIT", "QUIT":
		return syscall.SIGQUIT
	}
	return syscall.SIGTERM
}

const DefaultMemoryMax = 2 << 30

func UnitSpec(o v1alpha1.HostProcess, exec string) (hostsvc.UnitSpec, error) {
	s := o.Spec
	exec = cmp.Or(exec, s.Exec)
	if exec == "" {
		return hostsvc.UnitSpec{}, fmt.Errorf("hostprocess %s: neither an exec path nor a resolvable asset", o.Name)
	}
	desc := "HostProcess " + o.Name
	if d := s.Description; d != "" {
		desc = d
	}
	spec := hostsvc.UnitSpec{Name: s.Unit, Description: desc, Exec: append([]string{exec}, s.Argv...),
		WorkDir: s.WorkDir, Slice: s.Slice, Restart: restart(s.RestartPolicy), After: s.After, Wants: s.Wants,
		PrivateHostname: s.PrivateHostname, FDStore: int(s.FdStore), KillSignal: syscall.SIGTERM}
	for _, e := range s.Env {
		spec.Env = append(spec.Env, e.Name+"="+e.Value)
	}
	spec.EnvFiles = append([]string(nil), s.EnvFiles...)
	if s.Output != nil {
		spec.Output = hostsvc.Output{StdoutFile: s.Output.Stdout, StderrFile: s.Output.Stderr}
	}
	if s.Stop != nil {
		spec.KillSignal = signal(s.Stop.Signal)
		spec.StopTimeout = time.Duration(s.Stop.GraceSeconds) * time.Second
	}
	spec.Limits.MemoryMax = DefaultMemoryMax
	if s.Limits != nil {
		spec.Limits = hostsvc.Limits{MemoryMax: DefaultMemoryMax, CPUWeight: uint64(s.Limits.CpuWeight), CPUQuotaPercent: int(s.Limits.CpuQuotaPercent),
			TasksMax: uint64(s.Limits.TasksMax)}
		if s.Limits.MemoryMax != "" {
			n, err := strconv.ParseUint(s.Limits.MemoryMax, 10, 64)
			if err != nil {
				return hostsvc.UnitSpec{}, fmt.Errorf("hostprocess %s: limits.memoryMax %q is not a byte count", o.Name, s.Limits.MemoryMax)
			}
			spec.Limits.MemoryMax = n
		}
	}
	return spec, hostsvc.ValidateSpec(spec)
}

func lost(r hostsvc.LostReason) v1alpha1.HostProcessLostReason {
	switch r {
	case hostsvc.LostHostRebooted:
		return v1alpha1.HostProcessLostHostRebooted
	case hostsvc.LostUnitFailed, hostsvc.LostUnitExited:
		return v1alpha1.HostProcessLostUnitFailed
	case hostsvc.LostOOMKilled:
		return v1alpha1.HostProcessLostOOMKilled
	case hostsvc.LostIdentityMismatch:
		return v1alpha1.HostProcessLostIdentityMismatch
	}
	return v1alpha1.HostProcessLostUnitGone
}

func phaseOf(st hostsvc.UnitStatus) v1alpha1.HostProcessPhase {
	switch st.State {
	case hostsvc.Active:
		return v1alpha1.HostProcessPhaseRunning
	case hostsvc.Activating, hostsvc.Reloading:
		return v1alpha1.HostProcessPhaseStarting
	case hostsvc.Deactivating:
		return v1alpha1.HostProcessPhaseStopping
	case hostsvc.Failed:
		return v1alpha1.HostProcessPhaseFailed
	}
	return v1alpha1.HostProcessPhaseStopped
}

func Reconcile(o Observed) Result {
	obj := o.Object
	rt := obj.Status.Runtime
	if o.Inspect != nil {
		return Result{Phase: obj.Status.Phase, LostReason: obj.Status.LostReason, Unknown: true,
			Message: "the service manager is unreachable; Unknown, never Lost: " + o.Inspect.Error()}
	}
	if rt != nil && rt.Backend != "" && rt.Backend != string(o.Backend) {
		return Result{Phase: v1alpha1.HostProcessPhaseLost, LostReason: v1alpha1.HostProcessLostIdentityMismatch,
			Message: fmt.Sprintf("placed by the %s backend and this supervisor runs %s: never adopted across backends", rt.Backend, o.Backend)}
	}
	if obj.Spec.Placement == v1alpha1.HostProcessPlacementBootstrap {
		if o.Unit.State == hostsvc.NotFound || o.Unit.State == "" {
			return Result{Phase: v1alpha1.HostProcessPhaseLost, LostReason: v1alpha1.HostProcessLostUnitGone,
				Message: "the bootstrap unit is not loaded; its supervisor (systemd or devsup) owns it, this is a mirror"}
		}
		return Result{Phase: phaseOf(o.Unit), Identify: o.Unit.State == hostsvc.Active,
			Message: "a read-only mirror of a bootstrap-layer unit"}
	}
	if o.Prefix != "" && (len(obj.Spec.Unit) < len(o.Prefix) || obj.Spec.Unit[:len(o.Prefix)] != o.Prefix) {
		return Result{Phase: v1alpha1.HostProcessPhaseFailed, Message: fmt.Sprintf("%v: %s is not under %s", ErrNotOurs, obj.Spec.Unit, o.Prefix)}
	}
	if o.Deleting {
		if o.Unit.State == hostsvc.Active || o.Unit.State == hostsvc.Activating {
			return Result{Phase: v1alpha1.HostProcessPhaseStopping, Ops: []Op{{Kind: OpStop, Unit: obj.Spec.Unit}}}
		}
		return Result{Phase: v1alpha1.HostProcessPhaseStopped}
	}
	if rt != nil && rt.InvocationID != "" {
		if obj.Status.Phase == v1alpha1.HostProcessPhaseLost {
			return Result{Phase: v1alpha1.HostProcessPhaseLost, LostReason: obj.Status.LostReason, Message: obj.Status.Message}
		}
		if o.Restarted {
			message := fmt.Sprintf("restarted in place by its manager (restartPolicy %s, invocation %s, recorded %s)",
				obj.Spec.RestartPolicy, o.Unit.InvocationID, rt.InvocationID)
			if o.StartedKey != "" && o.StartedKey == obj.Spec.RestartKey && obj.Spec.RestartKey != "" {
				message = fmt.Sprintf("restarted in place for spec.restartKey %q (invocation %s, recorded %s)",
					obj.Spec.RestartKey, o.Unit.InvocationID, rt.InvocationID)
			}
			return Result{Phase: phaseOf(o.Unit), Identify: o.Unit.State == hostsvc.Active, Message: message}
		}
		if o.Check == nil {
			return Result{Phase: obj.Status.Phase, Unknown: true, Message: "the recorded identity was not checked"}
		}
		switch o.Check.Verdict {
		case hostsvc.VerdictUnknown:
			return Result{Phase: obj.Status.Phase, Unknown: true, Message: "Unknown: " + o.Check.Detail}
		case hostsvc.VerdictRunning:
			if obj.Spec.RestartKey != obj.Status.RestartKey {
				spec, err := UnitSpec(obj, o.Exec)
				if err != nil {
					return Result{Phase: v1alpha1.HostProcessPhaseFailed, Message: err.Error()}
				}
				return Result{Phase: v1alpha1.HostProcessPhaseStarting, Identify: true,
					Ops: []Op{{Kind: OpStop, Unit: obj.Spec.Unit}, {Kind: OpStart, Spec: spec, Unit: spec.Name}},
					Message: fmt.Sprintf("restarted in place: spec.restartKey is %q and the running process started under %q",
						obj.Spec.RestartKey, obj.Status.RestartKey)}
			}
			return Result{Phase: v1alpha1.HostProcessPhaseRunning, Identify: o.Check.Status.InvocationID != rt.InvocationID,
				Message: "adopted by unit name and InvocationID"}
		}
		if o.Check.Reason == hostsvc.LostUnitExited && o.Check.Status.ExecMainStatus == 0 && o.Check.Status.ExecMainCode <= 1 {
			return Result{Phase: v1alpha1.HostProcessPhaseStopped, Message: "the process exited 0"}
		}
		r := Result{Phase: v1alpha1.HostProcessPhaseLost, LostReason: lost(o.Check.Reason), Message: o.Check.Detail}
		if o.Check.Reason == hostsvc.LostIdentityMismatch && !o.Check.Stopped && o.Unit.State == hostsvc.Active {
			r.Ops = []Op{{Kind: OpStop, Unit: obj.Spec.Unit}}
		}
		return r
	}
	switch o.Unit.State {
	case hostsvc.Active:
		return Result{Phase: v1alpha1.HostProcessPhaseRunning, Identify: true, Message: "an active unit of this name is adopted"}
	case hostsvc.Activating:
		return Result{Phase: v1alpha1.HostProcessPhaseStarting, Identify: true}
	}
	if obj.Spec.RestartPolicy == v1alpha1.HostProcessRestartNever && obj.Status.Phase != "" && obj.Status.Phase != v1alpha1.HostProcessPhasePending {
		if o.Unit.State == hostsvc.Failed {
			return Result{Phase: v1alpha1.HostProcessPhaseLost, LostReason: v1alpha1.HostProcessLostUnitFailed,
				Message: fmt.Sprintf("the unit failed (result %s) before it was identified; restartPolicy Never places it once", o.Unit.Result)}
		}
		return Result{Phase: v1alpha1.HostProcessPhaseStopped, Message: "the unit ended before it was identified; restartPolicy Never places it once"}
	}
	spec, err := UnitSpec(obj, o.Exec)
	if err != nil {
		return Result{Phase: v1alpha1.HostProcessPhaseFailed, Message: err.Error()}
	}
	return Result{Phase: v1alpha1.HostProcessPhaseStarting, Ops: []Op{{Kind: OpStart, Spec: spec, Unit: spec.Name}}, Identify: true}
}

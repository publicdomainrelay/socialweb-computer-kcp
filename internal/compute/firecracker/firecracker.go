package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	conditionReasonOK = "Observed"

	PlaceTimeout = 20 * time.Second

	ReleaseKillAfter = 30 * time.Second

	MessageCap = 4000
)

type UserData interface {
	UserData(ctx context.Context, name string, ref commonv1.SecretRef) (string, bool, error)
}

type Placement interface {
	Place(ctx context.Context, vm *v1alpha1.FirecrackerVM, slot int, spec host.UnitSpec) error
	Placed(ctx context.Context, name string) (slot int, placed bool, err error)
	Unplace(ctx context.Context, name string) error
	Settle(ctx context.Context, name string) error
}

type Config struct {
	Handle string

	Log *slog.Logger
}

type Deps struct {
	Runtime vmruntime.Runtime

	Units host.Units

	UserData UserData

	Placement Placement
}

var (
	sizeRE    = regexp.MustCompile(`(?i)^\s*(\d+)\s*([KMGT])?i?B?\s*$`)
	vmmExitRE = regexp.MustCompile(`Firecracker exiting[^\n]*exit_code=(\d+)`)
)

type Provider struct {
	rt vmruntime.Runtime

	m host.Units

	userData UserData

	placement Placement

	handle string

	log *slog.Logger

	mu sync.Mutex

	exits map[string]hostsvc.UnitStatus
}

func New(cfg Config, deps Deps) (*Provider, error) {
	if cfg.Handle == "" {
		return nil, errors.New("firecracker: the provider names no handle; VM units and work dirs are namespaced by it")
	}
	if deps.Runtime == nil || deps.Units == nil || deps.UserData == nil || deps.Placement == nil {
		return nil, errors.New("firecracker: the provider needs a VM runtime, host units, a user data reader and a placement")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Provider{rt: deps.Runtime, m: deps.Units, userData: deps.UserData, placement: deps.Placement, handle: cfg.Handle,
		log: log.With("backend", v1alpha1.BackendKindFirecrackerVM, "handle", cfg.Handle), exits: map[string]hostsvc.UnitStatus{}}, nil
}

func (p *Provider) Handle() string { return p.handle }

func CapMessage(s string) string {
	if len(s) <= MessageCap {
		return s
	}
	return s[:MessageCap/2] + "\n...\n" + s[len(s)-MessageCap/2:]
}

func ParseMiB(s string) (int, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("firecracker: size %q is not <number>[K|M|G|T][i][B]", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	switch strings.ToUpper(m[2]) {
	case "K":
		return max(1, (n+512)/1024), nil
	case "G":
		return n * 1024, nil
	case "T":
		return n * 1024 * 1024, nil
	}
	return n, nil
}

func lostReason(r hostsvc.LostReason, exit *hostsvc.UnitStatus) v1alpha1.ComputeVMLostReason {
	if r == hostsvc.LostUnitGone && exit != nil {
		switch {
		case exit.State == hostsvc.Failed && exit.Result == "oom-kill":
			return v1alpha1.ComputeVMLostOOMKilled
		case exit.State == hostsvc.Failed:
			return v1alpha1.ComputeVMLostUnitFailed
		case exit.State == hostsvc.Inactive:
			return v1alpha1.ComputeVMLostVMMExited
		}
	}
	switch r {
	case hostsvc.LostHostRebooted:
		return v1alpha1.ComputeVMLostHostRebooted
	case hostsvc.LostUnitExited:
		return v1alpha1.ComputeVMLostVMMExited
	case hostsvc.LostUnitFailed:
		return v1alpha1.ComputeVMLostUnitFailed
	case hostsvc.LostOOMKilled:
		return v1alpha1.ComputeVMLostOOMKilled
	case hostsvc.LostIdentityMismatch:
		return v1alpha1.ComputeVMLostIdentityMismatch
	}
	return v1alpha1.ComputeVMLostUnitGone
}

func setCondition(st *v1alpha1.FirecrackerVMStatus, gen int64, t string, status metav1.ConditionStatus, reason, msg string) {
	conds.SetStatus(&st.Conditions, gen, t, status, reason, CapMessage(msg))
}

func ready(st *v1alpha1.FirecrackerVMStatus, gen int64) {
	ok := st.Phase == v1alpha1.ComputeVMPhaseRunning
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	setCondition(st, gen, v1alpha1.FirecrackerVMConditionReady, status, string(st.Phase), st.Message)
}

func Finish(st v1alpha1.FirecrackerVMStatus, gen int64) v1alpha1.FirecrackerVMStatus {
	st.Conditions = append([]metav1.Condition(nil), st.Conditions...)
	st.ObservedGeneration = gen
	st.Message = CapMessage(st.Message)
	ready(&st, gen)
	return st
}

func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

func (p *Provider) RefOf(vm *v1alpha1.FirecrackerVM) vmruntime.RuntimeRef {
	placed := p.rt.Placed(vm.Spec.Owner.Handle, vm.Name)
	ref := vmruntime.RuntimeRef{Handle: vm.Spec.Owner.Handle, Name: vm.Name, Unit: placed.Unit, VMMID: placed.VMMID,
		WorkDir: placed.WorkDir, Socket: placed.Socket}
	rt := vm.Status.Runtime
	if rt == nil || rt.InvocationID == "" {
		return ref
	}
	ref.Backend, ref.Hypervisor = hostsvc.Backend(rt.Backend), rt.Hypervisor
	ref.Unit = cmpString(rt.Unit, ref.Unit)
	ref.InvocationID, ref.PID, ref.StartTicks = rt.InvocationID, int(rt.Pid), uint64(rt.StartTicks)
	ref.HostBootID, ref.Manager, ref.VMMID = rt.HostBootID, rt.Manager, cmpString(rt.VMMID, ref.VMMID)
	ref.VMMUID, ref.Slot, ref.Socket = int(rt.VMMUID), int(rt.Slot), cmpString(rt.Socket, ref.Socket)
	ref.WorkDir = cmpString(vm.Status.WorkDir, ref.WorkDir)
	if t, err := time.Parse(time.RFC3339Nano, rt.StartTime); err == nil {
		ref.StartTime = t
	}
	if n := vm.Status.Network; n != nil {
		ref.GuestAddress, ref.HostAddress = n.GuestIP, n.HostAddress
	}
	if img := vm.Status.Image; img != nil {
		ref.ImageSHA256 = img.Sha256
	}
	return ref
}

func cmpString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func runningStatus(st v1alpha1.FirecrackerVMStatus, gen int64, ref vmruntime.RuntimeRef) v1alpha1.FirecrackerVMStatus {
	st.Phase, st.LostReason = v1alpha1.ComputeVMPhaseRunning, ""
	st.Network = &v1alpha1.FirecrackerVMNetwork{GuestIP: ref.GuestAddress, HostAddress: ref.HostAddress}
	st.Image = &v1alpha1.FirecrackerVMImage{Sha256: ref.ImageSHA256, Hypervisor: ref.Hypervisor}
	st.Runtime = &v1alpha1.FirecrackerVMRuntime{Backend: string(ref.Backend), Hypervisor: ref.Hypervisor, Unit: ref.Unit,
		InvocationID: ref.InvocationID, Pid: int64(ref.PID), StartTicks: int64(ref.StartTicks), StartTime: ref.StartTime.UTC().Format(time.RFC3339Nano),
		HostBootID: ref.HostBootID, Manager: ref.Manager, VMMID: ref.VMMID, VMMUID: int64(ref.VMMUID), Slot: int32(ref.Slot), Socket: ref.Socket}
	st.WorkDir = ref.WorkDir
	st.BootedAt = time.Now().UTC().Format(time.RFC3339Nano)
	st.Message = fmt.Sprintf("firecracker %s answers Running in unit %s (pid %d, slot %d)", ref.VMMID, ref.Unit, ref.PID, ref.Slot)
	setCondition(&st, gen, v1alpha1.ConditionStarted, metav1.ConditionTrue, "UnitStarted", ref.Unit)
	setCondition(&st, gen, v1alpha1.ConditionRuntimeObserved, metav1.ConditionTrue, conditionReasonOK, "the unit, the VMM pid and firecracker GET / match the record")
	return st
}

func (p *Provider) live(ctx context.Context, unit string) (bool, error) {
	st, err := p.m.Inspect(ctx, unit)
	if err != nil {
		return false, err
	}
	return hostsvc.Live(st) || st.State == hostsvc.Reloading, nil
}

func (p *Provider) placer(vm *v1alpha1.FirecrackerVM) vmruntime.Placer {
	return func(ctx context.Context, slot int, spec hostsvc.UnitSpec) error {
		if err := p.placement.Place(ctx, vm, slot, spec); err != nil {
			return err
		}
		p.log.Info("the VM unit is handed to its placement", "firecrackervm", vm.Name, "unit", spec.Name, "slot", slot)
		pctx, cancel := context.WithTimeout(ctx, PlaceTimeout)
		defer cancel()
		if _, err := hostsvc.Await(pctx, p.m, spec.Name, func(st hostsvc.UnitStatus) bool {
			return st.State != hostsvc.NotFound && st.State != ""
		}); err != nil {
			return fmt.Errorf("firecracker: %s was not placed within %s: %w", spec.Name, PlaceTimeout, err)
		}
		return nil
	}
}

func (p *Provider) fail(ctx context.Context, vm *v1alpha1.FirecrackerVM, st v1alpha1.FirecrackerVMStatus, why error) v1alpha1.FirecrackerVMStatus {
	placed := p.rt.Placed(vm.Spec.Owner.Handle, vm.Name)
	msg := why.Error()
	if t := tail(filepath.Join(placed.WorkDir, vmruntime.VMMLogFile), 1500) + tail(filepath.Join(placed.WorkDir, vmruntime.ConsoleFile), 1500); t != "" {
		msg += "\n" + t
	}
	p.log.Warn("failed", "firecrackervm", vm.Name, "err", why.Error())
	if err := p.teardown(ctx, vm.Name, placed.Unit); err != nil {
		p.log.Warn("cleaning up a failed boot", "firecrackervm", vm.Name, "err", err.Error())
	}
	st.Phase, st.LostReason, st.Message = v1alpha1.ComputeVMPhaseFailed, "", msg
	return st
}

func (p *Provider) Boot(ctx context.Context, vm *v1alpha1.FirecrackerVM, write func(v1alpha1.FirecrackerVMStatus) error) (v1alpha1.FirecrackerVMStatus, error) {
	h, log := vm.Spec.Owner.Handle, p.log.With("firecrackervm", vm.Name)
	st := vm.Status
	if ref, err := p.rt.Load(h, vm.Name); err == nil && ref.InvocationID != "" {
		log.Info("adopting the runtime record of a VM booted before this core")
		return runningStatus(st, vm.Generation, ref), nil
	}
	placed := p.rt.Placed(h, vm.Name)
	slot, placedBefore, err := p.placement.Placed(ctx, vm.Name)
	if err != nil {
		return st, err
	}
	if placedBefore {
		log.Info("the VM unit was placed before this core; awaiting its VMM", "slot", slot)
	} else {
		if live, err := p.live(ctx, placed.Unit); err != nil {
			return st, err
		} else if live {
			return p.fail(ctx, vm, st, fmt.Errorf("unit %s is running with no HostProcess child and no runtime record; it is not this FirecrackerVM's VM", placed.Unit)), nil
		}
		if _, err := os.Stat(placed.WorkDir); err == nil {
			log.Info("removing the work dir of an interrupted prepare", "workDir", placed.WorkDir)
			if err := p.rt.Cleanup(ctx, h, vm.Name); err != nil {
				return st, err
			}
		}
		userData, ok, err := p.userData.UserData(ctx, vm.Name, vm.Spec.UserData.Vault)
		if err != nil {
			return p.fail(ctx, vm, st, err), nil
		}
		if !ok {
			st.Phase = v1alpha1.ComputeVMPhasePending
			st.Message = "waiting for the user data at " + v1alpha1.VaultServer(vm.Spec.UserData.Vault.Vault, "") + " " +
				vm.Spec.UserData.Vault.Mount + "/" + vm.Spec.UserData.Vault.Path
			if err := write(st); err != nil {
				return st, err
			}
			return st, errors.New("firecracker: " + st.Message)
		}
		mem, err := ParseMiB(vm.Spec.Mem)
		if err != nil {
			log.Info("spec.mem does not parse; the guest gets the image's own memory", "mem", vm.Spec.Mem)
		}
		disk, err := ParseMiB(vm.Spec.Disk)
		if err != nil {
			log.Info("spec.disk does not parse; the guest keeps the image's own disk size", "disk", vm.Spec.Disk)
		}
		st.Phase, st.Message = v1alpha1.ComputeVMPhasePreparing, "image ensure, rootfs clone, seed drive"
		if err := write(st); err != nil {
			return st, err
		}
		started := time.Now()
		prep, err := p.rt.Prepare(ctx, vmruntime.VMSpec{Handle: h, Name: vm.Name, UserData: userData, Vcpu: int(vm.Spec.Vcpu), MemMiB: mem, DiskMiB: disk})
		if err != nil {
			return p.fail(ctx, vm, st, err), nil
		}
		var imageMiB int64
		if fi, err := os.Stat(filepath.Join(prep.ImageDir, guestimage.RootfsName)); err == nil {
			imageMiB = fi.Size() >> 20
		}
		log.Info("prepared", "workDir", prep.WorkDir, "vcpu", prep.Vcpu, "memMiB", prep.MemMiB, "diskMiB", disk, "imageRootfsMiB", imageMiB,
			"diskKeptAtImageSize", int64(disk) <= imageMiB, "seconds", time.Since(started).Seconds())
		setCondition(&st, vm.Generation, v1alpha1.ConditionPrepared, metav1.ConditionTrue, "Prepared", prep.WorkDir)
		st.WorkDir = prep.WorkDir
		placed = prep
		slot, err = p.rt.Reserve(ctx, placed, p.placer(vm))
		if err != nil {
			return p.fail(ctx, vm, st, err), nil
		}
	}
	st.Phase, st.Message = v1alpha1.ComputeVMPhaseBooting, "unit "+placed.Unit+" placed; waiting for firecracker GET / Running"
	setCondition(&st, vm.Generation, v1alpha1.ConditionStarted, metav1.ConditionTrue, "UnitStarted", placed.Unit)
	if err := write(st); err != nil {
		return st, err
	}
	ref, err := p.rt.Await(ctx, placed, slot)
	if err != nil {
		return p.fail(ctx, vm, st, err), nil
	}
	log.Info("running", "unit", ref.Unit, "vmmPid", ref.PID, "slot", ref.Slot, "vmmUid", ref.VMMUID, "invocation", ref.InvocationID)
	return runningStatus(st, vm.Generation, ref), nil
}

func (p *Provider) Observe(ctx context.Context, vm *v1alpha1.FirecrackerVM) v1alpha1.FirecrackerVMStatus {
	st := vm.Status
	ref := p.RefOf(vm)
	obs, err := p.rt.Inspect(ctx, ref)
	if err != nil && obs.Verdict != hostsvc.VerdictLost.String() {
		setCondition(&st, vm.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionUnknown, "Unknown", err.Error())
		return st
	}
	switch obs.Verdict {
	case hostsvc.VerdictRunning.String():
		setCondition(&st, vm.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionTrue, conditionReasonOK, "the unit, the VMM pid and firecracker GET / match the record")
		return st
	case hostsvc.VerdictLost.String():
		p.mu.Lock()
		exit, seen := p.exits[vm.Name]
		p.mu.Unlock()
		var ex *hostsvc.UnitStatus
		if seen {
			ex = &exit
		}
		reason := lostReason(obs.Reason, ex)
		msg := obs.Detail
		if reason == v1alpha1.ComputeVMLostUnitGone && ref.WorkDir != "" {
			logs := tail(filepath.Join(ref.WorkDir, vmruntime.VMMLogFile), 4096) + tail(filepath.Join(ref.WorkDir, vmruntime.ConsoleFile), 4096)
			if m := vmmExitRE.FindStringSubmatch(logs); m != nil {
				reason = v1alpha1.ComputeVMLostVMMExited
				msg = fmt.Sprintf("%s; the VMM logged %q", msg, m[0])
			}
		}
		if ex != nil {
			msg = fmt.Sprintf("%s; the unit watch saw it %s (result %s, exit code %d status %d)", msg, ex.State, ex.Result, ex.ExecMainCode, ex.ExecMainStatus)
		}
		if obs.Stopped {
			msg += "; the unit was stopped at once"
		}
		if err := p.m.ResetFailed(ctx, ref.Unit); err != nil {
			p.log.Info("reset-failed after a Lost verdict", "firecrackervm", vm.Name, "err", err.Error())
		}
		st.Phase, st.LostReason, st.Message = v1alpha1.ComputeVMPhaseLost, reason, msg
		setCondition(&st, vm.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionFalse, string(reason), msg)
		p.log.Warn("lost", "firecrackervm", vm.Name, "reason", string(reason), "detail", msg)
		return st
	}
	detail := obs.Detail
	if err != nil {
		detail = err.Error()
	}
	setCondition(&st, vm.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionUnknown, "Unknown", detail)
	return st
}

func dirSize(path string) int64 {
	var n int64
	filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (p *Provider) teardown(ctx context.Context, name, unit string) error {
	if err := p.placement.Unplace(ctx, name); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, ReleaseKillAfter)
	_, err := hostsvc.Await(wctx, p.m, unit, func(st hostsvc.UnitStatus) bool { return !hostsvc.Live(st) && st.State != hostsvc.Reloading })
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.log.Warn("the unit outlived its placement; stopping it here", "firecrackervm", name, "unit", unit, "err", err.Error())
		if err := p.m.Stop(ctx, unit); err != nil {
			return err
		}
	}
	p.m.ResetFailed(ctx, unit)
	if err := p.placement.Settle(ctx, name); err != nil {
		return err
	}
	placed := p.rt.Placed(p.handle, name)
	size := dirSize(placed.WorkDir)
	if err := p.rt.Cleanup(ctx, p.handle, name); err != nil {
		return err
	}
	for _, d := range []string{placed.WorkDir, placed.SocketDir} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("firecracker: %s is still there after cleanup (%v)", d, err)
		}
	}
	p.log.Info("work dir and API socket removed", "firecrackervm", name, "workDir", placed.WorkDir, "bytes", size)
	return nil
}

func (p *Provider) Teardown(ctx context.Context, vm *v1alpha1.FirecrackerVM) error {
	log := p.log.With("firecrackervm", vm.Name)
	ref := p.RefOf(vm)
	if live, err := p.live(ctx, ref.Unit); err != nil {
		return err
	} else if live {
		grace := p.rt.StopGrace()
		if vm.Spec.StopGraceSeconds > 0 {
			grace = time.Duration(vm.Spec.StopGraceSeconds) * time.Second
		}
		started := time.Now()
		graceful, err := p.rt.Shutdown(ctx, ref, grace)
		if err != nil {
			return err
		}
		log.Info("shutdown", "graceful", graceful, "seconds", time.Since(started).Seconds(), "grace", grace.String())
	}
	if err := p.teardown(ctx, vm.Name, ref.Unit); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.exits, vm.Name)
	p.mu.Unlock()
	log.Info("released")
	return nil
}

func (p *Provider) DropSlice(ctx context.Context) {
	listed, err := p.rt.List(ctx, p.handle)
	if err != nil {
		return
	}
	for _, l := range listed {
		if l.Loaded {
			return
		}
	}
	slice := vmruntime.SliceName(p.handle)
	if err := p.m.StopSlice(ctx, slice); err != nil {
		p.log.Info("stopping the empty per-handle slice", "slice", slice, "err", err.Error())
		return
	}
	p.log.Info("the per-handle slice is stopped: no VM unit of this handle is loaded", "slice", slice)
}

func (p *Provider) CollectImages(ctx context.Context, keep map[string]string) {
	rep, err := p.rt.CollectImages(ctx, keep)
	if err != nil {
		p.log.Warn("collecting guest images no FirecrackerVM, versions.json or binary names", "err", err.Error())
		return
	}
	if len(rep.Removed) > 0 {
		p.log.Info("guest images no FirecrackerVM, versions.json entry or current or previous binary names are removed",
			"removed", rep.Removed, "kept", len(rep.Kept))
	}
}

func (p *Provider) Listed(ctx context.Context) []vmruntime.Listed {
	listed, err := p.rt.List(ctx, p.handle)
	if err != nil {
		p.log.Warn("listing VM units and work dirs", "err", err.Error())
		return nil
	}
	var out []vmruntime.Listed
	for _, l := range listed {
		if l.WorkDir != "" {
			out = append(out, l)
		}
	}
	return out
}

func (p *Provider) Live(ctx context.Context, name string) (bool, error) {
	return p.live(ctx, vmruntime.UnitName(p.handle, name))
}

func (p *Provider) Collect(ctx context.Context, l vmruntime.Listed) error {
	if l.Loaded {
		if err := p.m.Stop(ctx, l.Unit); err != nil {
			return err
		}
		p.log.Warn("orphan VM unit stopped", "unit", l.Unit)
	}
	return p.teardown(ctx, l.Name, l.Unit)
}

func (p *Provider) Watch(ctx context.Context) (<-chan string, error) {
	ch, err := p.rt.Watch(ctx, p.handle)
	if err != nil {
		return nil, err
	}
	prefix, suffix := vmruntime.UnitPrefix(p.handle), hostsvc.ServiceSuffix
	out := make(chan string, 64)
	go func() {
		defer close(out)
		for ev := range ch {
			name := strings.TrimSuffix(strings.TrimPrefix(ev.Name, prefix), suffix)
			if ev.Status.State == hostsvc.Inactive || ev.Status.State == hostsvc.Failed {
				p.mu.Lock()
				p.exits[name] = ev.Status
				p.mu.Unlock()
				p.log.Info("the unit watch saw a VM unit end", "firecrackervm", name, "state", string(ev.Status.State),
					"result", ev.Status.Result, "exitCode", ev.Status.ExecMainStatus)
			}
			select {
			case out <- name:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

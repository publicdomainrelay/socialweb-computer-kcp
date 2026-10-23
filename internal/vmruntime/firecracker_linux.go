//go:build linux

package vmruntime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/fcapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestdisk"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

type firecracker struct {
	cfg    Config
	subuid hostcontract.SubidEntry
	subgid hostcontract.SubidEntry
	slots  int
	target string
}

func ForHost(cfg Config) (Runtime, error) {
	if cfg.Manager == nil {
		return nil, fmt.Errorf("vmruntime: no placement manager")
	}
	if !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.RuntimeDir) {
		return nil, fmt.Errorf("vmruntime: state dir %q and runtime dir %q must be absolute", cfg.StateDir, cfg.RuntimeDir)
	}
	for name, p := range map[string]string{"swc-userns-stage": cfg.Exec.Stage, "pasta": cfg.Exec.Pasta,
		"socialweb-vmexec": cfg.Exec.VMExec, "firecracker": cfg.Exec.Firecracker} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("vmruntime: %s path %q is not absolute", name, p)
		}
	}
	cfg.InnerID = cmp.Or(cfg.InnerID, DefaultInnerID)
	if cfg.InnerID <= 0 || cfg.InnerID >= hostcontract.VMSlotIDs {
		return nil, fmt.Errorf("vmruntime: inner id %d must be inside 1..%d: SELinux refuses pasta as uid 0 of the namespace", cfg.InnerID, hostcontract.VMSlotIDs-1)
	}
	cfg.BootTimeout = cmp.Or(cfg.BootTimeout, DefaultBootTime)
	cfg.StopGrace = cmp.Or(cfg.StopGrace, DefaultStopGrace)
	cfg.ConsoleCap = cmp.Or(cfg.ConsoleCap, DefaultConsoleCap)
	target := ImageTarget()
	if target == "" {
		return nil, ErrUnsupported
	}
	me, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("vmruntime: %w", err)
	}
	uid := os.Getuid()
	subuidText, err := os.ReadFile("/etc/subuid")
	if err != nil {
		return nil, fmt.Errorf("vmruntime: %w", err)
	}
	subgidText, err := os.ReadFile("/etc/subgid")
	if err != nil {
		return nil, fmt.Errorf("vmruntime: %w", err)
	}
	su, okU := hostcontract.AccountRange(string(subuidText), me.Username, uid)
	sg, okG := hostcontract.AccountRange(string(subgidText), me.Username, uid)
	if !okU || !okG {
		return nil, fmt.Errorf("vmruntime: %s has no subordinate id range in /etc/subuid and /etc/subgid; each VM runs in a user namespace mapped to a %d-id slot of it (socialweb-computer install bidder reserves %s:<base>:%d)",
			me.Username, hostcontract.VMSlotIDs, me.Username, 256*hostcontract.VMSlotIDs)
	}
	slots := int(min(su.Count, sg.Count) / hostcontract.VMSlotIDs)
	if slots < 1 {
		return nil, fmt.Errorf("vmruntime: the subordinate range of %s holds %d ids, under one %d-id slot", me.Username, min(su.Count, sg.Count), hostcontract.VMSlotIDs)
	}
	return &firecracker{cfg: cfg, subuid: su, subgid: sg, slots: slots, target: target}, nil
}

func (r *firecracker) Capabilities() Capabilities {
	c := r.cfg.Manager.Capabilities()
	return Capabilities{Backend: c.Backend, Hypervisor: "firecracker", EnforcesLimits: c.EnforcesLimits, GracefulStop: GracefulStop()}
}

func (r *firecracker) imagePin() (guestimage.Pin, error) { return guestimage.PinFor(r.target) }

func (r *firecracker) imageStore() guestimage.Store {
	return guestimage.Store{Root: filepath.Join(r.cfg.StateDir, "images"), Out: r.cfg.ImageOut, TTY: r.cfg.ImageTTY}
}

func (r *firecracker) Prepare(ctx context.Context, spec VMSpec) (Prepared, error) {
	if err := ValidateSpec(spec); err != nil {
		return Prepared{}, err
	}
	pin, err := r.imagePin()
	if err != nil {
		return Prepared{}, err
	}
	store := r.imageStore()
	img, err := store.Ensure(ctx, pin, r.cfg.ImageSource)
	if err != nil {
		return Prepared{}, err
	}
	manifest, err := readManifest(img.Dir)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{
		Spec:      spec,
		Unit:      UnitName(spec.Handle, spec.Name),
		WorkDir:   WorkDir(r.cfg.StateDir, spec.Handle, spec.Name),
		SocketDir: SocketDir(r.cfg.RuntimeDir, spec.Handle, spec.Name),
		VMMID:     VMMID(spec.Handle, spec.Name),
		ImageDir:  img.Dir,
		Vcpu:      manifest.Vcpu,
		MemMiB:    manifest.MemMiB,
	}
	p.Socket = filepath.Join(p.SocketDir, SocketFile)
	if len(p.Socket) >= 108 {
		return Prepared{}, fmt.Errorf("vmruntime: the API socket %s is %d bytes, over the 107-byte unix socket limit", p.Socket, len(p.Socket))
	}
	if spec.Vcpu > 0 {
		p.Vcpu = spec.Vcpu
	}
	if spec.MemMiB > 0 {
		p.MemMiB = spec.MemMiB
	}
	if _, err := os.Stat(p.WorkDir); err == nil {
		return Prepared{}, fmt.Errorf("vmruntime: %s already has a work directory %s; clean it up first", spec.Name, p.WorkDir)
	}
	if err := os.MkdirAll(filepath.Dir(p.WorkDir), 0o700); err != nil {
		return Prepared{}, fmt.Errorf("vmruntime: %w", err)
	}
	if err := os.Mkdir(p.WorkDir, 0o700); err != nil {
		return Prepared{}, fmt.Errorf("vmruntime: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(p.WorkDir)
		}
	}()
	rootfs := filepath.Join(p.WorkDir, RootfsFile)
	if err := guestdisk.Clone(filepath.Join(img.Dir, guestimage.RootfsName), rootfs); err != nil {
		return Prepared{}, err
	}
	if err := guestdisk.GrowTo(rootfs, spec.DiskMiB); err != nil {
		return Prepared{}, err
	}
	meta := spec.MetaData
	meta = cmp.Or(meta, "instance-id: "+spec.Name+"\nlocal-hostname: "+spec.Name+"\n")
	seed := filepath.Join(p.WorkDir, SeedFile)
	if err := guestdisk.WriteSeed(seed, []byte(spec.UserData), []byte(meta), []byte(guestnet.NetworkConfig())); err != nil {
		return Prepared{}, err
	}
	conf := fcapi.Config{
		BootSource: fcapi.BootSource{KernelImagePath: filepath.Join(img.Dir, guestimage.KernelName), BootArgs: strings.Join(manifest.BootArgs, " ")},
		Drives: []fcapi.Drive{
			{DriveID: "rootfs", PathOnHost: rootfs, IsRootDevice: true},
			{DriveID: "cidata", PathOnHost: seed, IsReadOnly: true},
		},
		MachineConfig:     fcapi.MachineConfig{VcpuCount: p.Vcpu, MemSizeMiB: p.MemMiB},
		NetworkInterfaces: []fcapi.NetworkInterface{{IfaceID: guestnet.Interface, HostDevName: guestnet.Tap, GuestMAC: guestnet.GuestMAC}},
	}
	if err := fcapi.WriteConfig(filepath.Join(p.WorkDir, ConfigFile), conf); err != nil {
		return Prepared{}, fmt.Errorf("vmruntime: %w", err)
	}
	console := filepath.Join(p.WorkDir, ConsoleFile)
	params := guestnet.VMExec{
		Wiring:  guestnet.Plan(),
		VMM:     r.cfg.Exec.Firecracker,
		Argv:    fcapi.Argv("firecracker", p.VMMID, filepath.Join(p.WorkDir, ConfigFile), p.Socket),
		Console: console,
	}
	if err := fsx.WriteJSON(filepath.Join(p.WorkDir, ParamsFile), params, 0o600); err != nil {
		return Prepared{}, err
	}
	for _, f := range []string{console, filepath.Join(p.WorkDir, VMMLogFile)} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			return Prepared{}, fmt.Errorf("vmruntime: %w", err)
		}
	}
	ok = true
	return p, nil
}

func (r *firecracker) slotIDs(k int) (uidBase, gidBase, vmmUID int) {
	uidBase = int(r.subuid.Start) + k*hostcontract.VMSlotIDs
	gidBase = int(r.subgid.Start) + k*hostcontract.VMSlotIDs
	return uidBase, gidBase, uidBase + r.cfg.InnerID
}

func (r *firecracker) UnitSpec(p Prepared, slot int) hostsvc.UnitSpec {
	uidBase, gidBase, _ := r.slotIDs(slot)
	exec := []string{r.cfg.Exec.Stage, strconv.Itoa(uidBase), strconv.Itoa(gidBase), strconv.Itoa(hostcontract.VMSlotIDs), strconv.Itoa(r.cfg.InnerID)}
	exec = append(exec, guestnet.PastaArgv(r.cfg.Exec.Pasta, r.cfg.Exec.VMExec, filepath.Join(p.WorkDir, ParamsFile))...)
	return hostsvc.UnitSpec{
		Name:        p.Unit,
		Description: fmt.Sprintf("socialweb vm %s handle %s slot=%d", p.Spec.Name, HandleID(p.Spec.Handle), slot),
		Exec:        exec,
		Env:         []string{"TZ=UTC"},
		WorkDir:     p.WorkDir,
		Output:      hostsvc.Output{StdoutFile: filepath.Join(p.WorkDir, VMMLogFile), StderrFile: filepath.Join(p.WorkDir, VMMLogFile)},
		Limits: hostsvc.Limits{
			MemoryMax:       uint64(p.MemMiB+VMMOverheadMiB) << 20,
			CPUWeight:       100,
			CPUQuotaPercent: 100 * p.Vcpu,
			TasksMax:        uint64(64 + 4*p.Vcpu),
		},
		OOMPolicy:   "stop",
		KillSignal:  syscall.SIGKILL,
		StopTimeout: StopTimeout,
		Slice:       SliceName(p.Spec.Handle),
		Restart:     hostsvc.RestartNo,
	}
}

func (r *firecracker) Start(ctx context.Context, p Prepared) (RuntimeRef, error) {
	slot, err := r.Reserve(ctx, p, func(ctx context.Context, _ int, spec hostsvc.UnitSpec) error {
		_, err := r.cfg.Manager.Start(ctx, spec)
		return err
	})
	ref := RuntimeRef{}
	if err == nil {
		ref, err = r.Await(ctx, p, slot)
	}
	if err != nil {
		stopCtx := context.WithoutCancel(ctx)
		_ = r.cfg.Manager.Stop(stopCtx, p.Unit)
		_ = r.cfg.Manager.ResetFailed(stopCtx, p.Unit)
		tail := tailOf(filepath.Join(p.WorkDir, VMMLogFile), 3000) + tailOf(filepath.Join(p.WorkDir, ConsoleFile), 3000)
		r.Cleanup(stopCtx, p.Spec.Handle, p.Spec.Name)
		return RuntimeRef{}, fmt.Errorf("%w\n%s", err, tail)
	}
	return ref, nil
}

func (r *firecracker) Placed(handle, name string) Prepared {
	p := Prepared{
		Spec:      VMSpec{Handle: handle, Name: name},
		Unit:      UnitName(handle, name),
		WorkDir:   WorkDir(r.cfg.StateDir, handle, name),
		SocketDir: SocketDir(r.cfg.RuntimeDir, handle, name),
		VMMID:     VMMID(handle, name),
	}
	p.Socket = filepath.Join(p.SocketDir, SocketFile)
	return p
}

func (r *firecracker) StopGrace() time.Duration { return r.cfg.StopGrace }

func (r *firecracker) awaitDown(ctx context.Context, unit string, grace time.Duration) (bool, error) {
	gctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	_, err := hostsvc.Await(gctx, r.cfg.Manager, unit, func(st hostsvc.UnitStatus) bool { return !hostsvc.Live(st) })
	switch {
	case err == nil:
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case gctx.Err() != nil:
		return false, nil
	}
	return false, err
}

func (r *firecracker) Shutdown(ctx context.Context, ref RuntimeRef, grace time.Duration) (bool, error) {
	st, err := r.cfg.Manager.Inspect(ctx, ref.Unit)
	if err != nil {
		return false, err
	}
	if !hostsvc.Live(st) {
		return true, nil
	}
	if !GracefulStop() || ref.Socket == "" {
		return false, nil
	}
	if err := fcapi.NewClient(ref.Socket, 2*time.Second).SendCtrlAltDel(ctx); err != nil {
		return false, nil
	}
	return r.awaitDown(ctx, ref.Unit, grace)
}

func (r *firecracker) Reserve(ctx context.Context, p Prepared, place Placer) (int, error) {
	unlock, err := r.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	used, err := r.usedSlots(ctx)
	if err != nil {
		return 0, err
	}
	slot := -1
	for k := 0; k < r.slots; k++ {
		if !used[k] {
			slot = k
			break
		}
	}
	if slot < 0 {
		return 0, fmt.Errorf("vmruntime: all %d VM slots of this account are held by loaded VM units", r.slots)
	}
	_, _, vmmUID := r.slotIDs(slot)
	if err := r.grant(p, vmmUID); err != nil {
		return 0, err
	}
	if err := place(ctx, slot, r.UnitSpec(p, slot)); err != nil {
		return 0, err
	}
	return slot, nil
}

func (r *firecracker) grant(p Prepared, vmmUID int) error {
	if err := os.MkdirAll(filepath.Dir(p.SocketDir), 0o700); err != nil {
		return fmt.Errorf("vmruntime: %w", err)
	}
	if err := os.Mkdir(p.SocketDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("vmruntime: %w", err)
	}
	g := newGranter(vmmUID)
	files := []struct {
		path string
		perm uint16
	}{
		{filepath.Join(p.WorkDir, RootfsFile), 6},
		{filepath.Join(p.WorkDir, SeedFile), 4},
		{filepath.Join(p.WorkDir, ConfigFile), 4},
		{filepath.Join(p.WorkDir, ParamsFile), 4},
		{filepath.Join(p.WorkDir, ConsoleFile), 6},
		{p.SocketDir, 7},
	}
	for _, f := range files {
		if err := g.need(f.path, f.perm, false); err != nil {
			return err
		}
	}
	if err := g.need(p.WorkDir, 1, false); err != nil {
		return err
	}
	if err := g.traverse(p.WorkDir, ""); err != nil {
		return err
	}
	if err := g.traverse(p.SocketDir, ""); err != nil {
		return err
	}
	for _, f := range []string{filepath.Join(p.ImageDir, guestimage.KernelName), r.cfg.Exec.Pasta, r.cfg.Exec.VMExec, r.cfg.Exec.Firecracker} {
		if err := g.traverse(f, ""); err != nil {
			return err
		}
	}
	for _, f := range []string{r.cfg.Exec.Pasta, r.cfg.Exec.VMExec, r.cfg.Exec.Firecracker} {
		if err := g.need(f, 5, true); err != nil {
			return err
		}
	}
	if err := g.need(filepath.Join(p.ImageDir, guestimage.KernelName), 4, true); err != nil {
		return err
	}
	var outside []Grant
	for _, gr := range g.grants {
		if !strings.HasPrefix(gr.Path, p.WorkDir+string(filepath.Separator)) && gr.Path != p.WorkDir {
			outside = append(outside, gr)
		}
	}
	return fsx.WriteJSON(filepath.Join(p.WorkDir, GrantsFile), outside, 0o600)
}

const (
	DialRetryMin = 10 * time.Millisecond
	DialRetryMax = time.Second
)

func (r *firecracker) awaitBoot(ctx context.Context, p Prepared) error {
	bctx, cancel := context.WithTimeout(ctx, r.cfg.BootTimeout)
	defer cancel()
	sock, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("vmruntime: watching %s: %w", p.SocketDir, err)
	}
	defer sock.Close()
	if err := sock.Add(p.SocketDir); err != nil {
		return fmt.Errorf("vmruntime: watching %s: %w", p.SocketDir, err)
	}
	units, _ := r.cfg.Manager.Watch(bctx, p.Unit)
	exited := func(st hostsvc.UnitStatus) error {
		if st.State != hostsvc.Active && st.State != hostsvc.Activating {
			return fmt.Errorf("vmruntime: %s went %s (result %s, status %d) before its VMM answered", p.Unit, st.State, st.Result, st.ExecMainStatus)
		}
		return nil
	}
	if st, err := r.cfg.Manager.Inspect(bctx, p.Unit); err == nil {
		if err := exited(st); err != nil {
			return err
		}
	}
	client := fcapi.NewClient(p.Socket, 2*time.Second)
	last := fmt.Errorf("the API socket %s is not there", p.Socket)
	delay := DialRetryMin
	var again <-chan time.Time
	check := true
	for {
		if check && fsx.Exists(p.Socket) {
			in, err := client.Describe(bctx)
			switch {
			case err == nil && in.ID == p.VMMID && in.State == fcapi.StateRunning:
				return nil
			case err != nil:
				last = err
				again = time.After(delay)
				delay = min(2*delay, DialRetryMax)
			default:
				last = fmt.Errorf("the VMM answers id %q state %q", in.ID, in.State)
			}
		}
		check = false
		select {
		case <-bctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("vmruntime: %s: the VMM was not Running within %s: %v", p.Unit, r.cfg.BootTimeout, last)
		case ev, ok := <-units:
			if !ok {
				units = nil
				continue
			}
			if ev.Name == p.Unit {
				if err := exited(ev.Status); err != nil {
					return err
				}
			}
			check = true
		case ev, ok := <-sock.Events:
			if !ok {
				return fmt.Errorf("vmruntime: the watch of %s ended", p.SocketDir)
			}
			check = filepath.Clean(ev.Name) == filepath.Clean(p.Socket)
		case err, ok := <-sock.Errors:
			if !ok {
				return fmt.Errorf("vmruntime: the watch of %s ended", p.SocketDir)
			}
			last, check = err, true
		case <-again:
			again, check = nil, true
		}
	}
}

func (r *firecracker) Await(ctx context.Context, p Prepared, slot int) (RuntimeRef, error) {
	if err := r.awaitBoot(ctx, p); err != nil {
		return RuntimeRef{}, err
	}
	st, err := r.cfg.Manager.Inspect(ctx, p.Unit)
	if err != nil {
		return RuntimeRef{}, err
	}
	pid, err := vmmPID(st.MainPID)
	if err != nil {
		return RuntimeRef{}, err
	}
	id, err := r.cfg.Manager.Identify(ctx, p.Unit, pid)
	if err != nil {
		return RuntimeRef{}, err
	}
	_, _, vmmUID := r.slotIDs(slot)
	pin, _ := r.imagePin()
	ref := RuntimeRef{
		Backend: id.Backend, Hypervisor: "firecracker", Handle: p.Spec.Handle, Name: p.Spec.Name,
		Unit: p.Unit, InvocationID: id.InvocationID, PID: id.PID, StartTicks: id.StartTicks,
		StartTime: time.Now().UTC(), HostBootID: id.BootID, Manager: id.Manager, VMMID: p.VMMID,
		Slot: slot, VMMUID: vmmUID, WorkDir: p.WorkDir, Socket: p.Socket,
		GuestAddress: guestnet.GuestAddress, HostAddress: guestnet.HostAddress,
		ImageSHA256: pin.Files[guestimage.ManifestName].SHA256,
	}
	if err := fsx.WriteJSON(filepath.Join(p.WorkDir, RefFile), ref, 0o600); err != nil {
		return RuntimeRef{}, err
	}
	return ref, nil
}

func identity(ref RuntimeRef) hostsvc.Identity {
	return hostsvc.Identity{Unit: ref.Unit, InvocationID: ref.InvocationID, PID: ref.PID, StartTicks: ref.StartTicks,
		BootID: ref.HostBootID, Backend: ref.Backend, Manager: ref.Manager}
}

func (r *firecracker) Inspect(ctx context.Context, ref RuntimeRef) (Observation, error) {
	obs, err := r.cfg.Manager.Check(ctx, identity(ref))
	out := Observation{Verdict: obs.Verdict.String(), Reason: obs.Reason, Detail: obs.Detail, Stopped: obs.Stopped, State: string(obs.Status.State)}
	if err != nil || obs.Verdict != hostsvc.VerdictRunning {
		return out, err
	}
	r.rotate(ref.WorkDir)
	in, err := fcapi.NewClient(ref.Socket, 2*time.Second).Describe(ctx)
	if err != nil {
		return Observation{Verdict: hostsvc.VerdictUnknown.String(), Detail: err.Error(), State: out.State}, nil
	}
	if in.ID != ref.VMMID || in.State != fcapi.StateRunning {
		out = Observation{Verdict: hostsvc.VerdictLost.String(), Reason: hostsvc.LostIdentityMismatch, State: out.State,
			Detail: fmt.Sprintf("the VMM on %s answers id %q state %q, recorded %q Running", ref.Socket, in.ID, in.State, ref.VMMID)}
		if err := r.cfg.Manager.Stop(ctx, ref.Unit); err != nil {
			return out, fmt.Errorf("vmruntime: %s does not match its record and stopping it failed: %w", ref.Unit, err)
		}
		out.Stopped = true
	}
	return out, nil
}

func (r *firecracker) Stop(ctx context.Context, ref RuntimeRef) (StopResult, error) {
	start := time.Now()
	res := StopResult{}
	st, err := r.cfg.Manager.Inspect(ctx, ref.Unit)
	if err != nil {
		return res, err
	}
	if GracefulStop() && (st.State == hostsvc.Active || st.State == hostsvc.Activating) {
		if err := fcapi.NewClient(ref.Socket, 2*time.Second).SendCtrlAltDel(ctx); err == nil {
			if res.Graceful, err = r.awaitDown(ctx, ref.Unit, r.cfg.StopGrace); err != nil {
				return res, err
			}
		}
	}
	if err := r.cfg.Manager.Stop(ctx, ref.Unit); err != nil {
		return res, err
	}
	if st, err := r.cfg.Manager.Inspect(ctx, ref.Unit); err == nil {
		res.Result = st.Result
	}
	if err := r.cfg.Manager.ResetFailed(ctx, ref.Unit); err != nil {
		return res, err
	}
	res.Elapsed = time.Since(start)
	return res, r.Cleanup(ctx, ref.Handle, ref.Name)
}

func (r *firecracker) Cleanup(ctx context.Context, handle, name string) error {
	unit := UnitName(handle, name)
	st, err := r.cfg.Manager.Inspect(ctx, unit)
	if err != nil {
		return err
	}
	if st.State == hostsvc.Active || st.State == hostsvc.Activating || st.State == hostsvc.Deactivating {
		return fmt.Errorf("vmruntime: %s is %s; stop it before its work directory goes", unit, st.State)
	}
	work := WorkDir(r.cfg.StateDir, handle, name)
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var errs []error
	var grants []Grant
	if raw, err := os.ReadFile(filepath.Join(work, GrantsFile)); err == nil {
		if err := json.Unmarshal(raw, &grants); err != nil {
			errs = append(errs, err)
		}
	}
	if err := revokeAll(grants); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(SocketDir(r.cfg.RuntimeDir, handle, name)); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(work); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *firecracker) List(ctx context.Context, handle string) ([]Listed, error) {
	units, err := r.cfg.Manager.List(ctx, UnitPrefix(handle))
	if err != nil {
		return nil, err
	}
	byName := map[string]*Listed{}
	var order []string
	for _, u := range units {
		name := strings.TrimSuffix(strings.TrimPrefix(u.Name, UnitPrefix(handle)), hostsvc.ServiceSuffix)
		byName[name] = &Listed{Name: name, Unit: u.Name, Status: u, Loaded: true}
		order = append(order, name)
	}
	entries, err := os.ReadDir(VMsDir(r.cfg.StateDir, handle))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		l, ok := byName[e.Name()]
		if !ok {
			l = &Listed{Name: e.Name(), Unit: UnitName(handle, e.Name())}
			byName[e.Name()] = l
			order = append(order, e.Name())
		}
		l.WorkDir = filepath.Join(VMsDir(r.cfg.StateDir, handle), e.Name())
	}
	out := make([]Listed, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

func (r *firecracker) Watch(ctx context.Context, handle string) (<-chan hostsvc.UnitEvent, error) {
	return r.cfg.Manager.Watch(ctx, UnitPrefix(handle))
}

func (r *firecracker) Load(handle, name string) (RuntimeRef, error) {
	var ref RuntimeRef
	raw, err := os.ReadFile(filepath.Join(WorkDir(r.cfg.StateDir, handle, name), RefFile))
	if err != nil {
		return ref, err
	}
	return ref, json.Unmarshal(raw, &ref)
}

func (r *firecracker) slotLockPath() (string, error) {
	account := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	if st, err := os.Stat(account); err == nil && st.IsDir() {
		return filepath.Join(account, "socialweb-vmslots.lock"), nil
	}
	if err := os.MkdirAll(r.cfg.StateDir, 0o700); err != nil {
		return "", fmt.Errorf("vmruntime: %w", err)
	}
	return filepath.Join(r.cfg.StateDir, "vmslots.lock"), nil
}

func (r *firecracker) lock() (func(), error) {
	path, err := r.slotLockPath()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vmruntime: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("vmruntime: locking the VM slots: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (r *firecracker) usedSlots(ctx context.Context) (map[int]bool, error) {
	units, err := r.cfg.Manager.List(ctx, UnitKind)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, u := range units {
		i := strings.LastIndex(u.Description, "slot=")
		if i < 0 {
			continue
		}
		if k, err := strconv.Atoi(u.Description[i+len("slot="):]); err == nil {
			used[k] = true
		}
	}
	return used, nil
}

func (r *firecracker) rotate(work string) {
	for _, name := range []string{ConsoleFile, VMMLogFile} {
		path := filepath.Join(work, name)
		st, err := os.Stat(path)
		if err != nil || st.Size() <= r.cfg.ConsoleCap {
			continue
		}
		in, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := in.Seek(-r.cfg.ConsoleCap, io.SeekEnd); err == nil {
			if out, err := os.OpenFile(path+".1", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err == nil {
				io.Copy(out, in)
				out.Close()
			}
		}
		in.Close()
		os.Truncate(path, 0)
	}
}

func vmmPID(pastaPID int) (int, error) {
	if pastaPID <= 0 {
		return 0, fmt.Errorf("vmruntime: the unit has no main pid")
	}
	tasks, _ := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pastaPID), "task"))
	for _, t := range tasks {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pastaPID), "task", t.Name(), "children"))
		if err != nil {
			continue
		}
		for f := range strings.FieldsSeq(string(raw)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
			comm, _ := os.ReadFile(filepath.Join("/proc", f, "comm"))
			if strings.TrimSpace(string(comm)) == "firecracker" {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("vmruntime: no firecracker child under pasta pid %d", pastaPID)
}

type imageManifest struct {
	BootArgs []string `json:"bootArgs"`
	Vcpu     int      `json:"vcpu"`
	MemMiB   int      `json:"memMiB"`
}

func readManifest(dir string) (imageManifest, error) {
	var m imageManifest
	raw, err := os.ReadFile(filepath.Join(dir, guestimage.ManifestName))
	if err != nil {
		return m, fmt.Errorf("vmruntime: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("vmruntime: %s: %w", dir, err)
	}
	if len(m.BootArgs) == 0 || m.Vcpu <= 0 || m.MemMiB <= 0 {
		return m, fmt.Errorf("vmruntime: the image manifest in %s names no boot args or machine size", dir)
	}
	return m, nil
}

func tailOf(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return "--- " + filepath.Base(path) + " ---\n" + string(b)
}

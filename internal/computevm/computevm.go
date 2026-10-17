package computevm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

const (
	ChildPrefix       = "vm-"
	LabelComputeVM    = "socialweb.computer/computevm"
	BidderSet         = "bidder"
	DefaultInterval   = 5 * time.Second
	SupervisorEvery   = time.Second
	RetryAfter        = 3 * time.Second
	OrphanGrace       = 2 * time.Minute
	PlaceTimeout      = 20 * time.Second
	ReleaseKillAfter  = 30 * time.Second
	MessageCap        = 4000
	ApplyTimeout      = time.Minute
	ImageGCEvery      = 10 * time.Minute
	conditionReasonOK = "Observed"
)

var (
	GVR       = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "computevms"}
	sizeRE    = regexp.MustCompile(`(?i)^\s*(\d+)\s*([KMGT])?i?B?\s*$`)
	slotRE    = regexp.MustCompile(`slot=(\d+)`)
	vmmExitRE = regexp.MustCompile(`Firecracker exiting[^\n]*exit_code=(\d+)`)
)

type Config struct {
	Kubeconfig  string
	Workspace   string
	Handle      string
	Runtime     vmruntime.Runtime
	Manager     hostsvc.Manager
	Interval    time.Duration
	OrphanGrace time.Duration
	VaultServer string
	VaultClient string
	VaultMount  string
	Log         *slog.Logger
}

type Controller struct {
	c       Config
	vms     dynamic.ResourceInterface
	hps     dynamic.ResourceInterface
	servers dynamic.ResourceInterface
	bids    dynamic.ResourceInterface
	grants  dynamic.ResourceInterface
	kick    chan string

	mu       sync.Mutex
	busy     map[string]bool
	exits    map[string]hostsvc.UnitStatus
	orphans  map[string]time.Time
	foreign  map[string]bool
	logins   map[string]vaultLogin
	slice    bool
	imagesAt time.Time
	bidsAt   time.Time
	linksAt  time.Time
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "computevm", "handle", c.Handle)
}

func (c Config) grace() time.Duration {
	if c.OrphanGrace == 0 {
		return OrphanGrace
	}
	return c.OrphanGrace
}

func ChildName(name string) string { return ChildPrefix + name }

func New(ctx context.Context, c Config) (*Controller, error) {
	if c.Handle == "" {
		return nil, errors.New("computevm: the controller names no handle; VM units and work dirs are namespaced by it")
	}
	if c.Runtime == nil || c.Manager == nil {
		return nil, errors.New("computevm: no VM runtime")
	}
	if c.Workspace == "" {
		c.Workspace = hostprocess.DefaultWorkspace
	}
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if err := applySets(ctx, c, restCfg); err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + c.Workspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Controller{c: c, vms: dc.Resource(GVR), hps: dc.Resource(hostprocess.GVR),
		servers: dc.Resource(openbaoctl.GVR), bids: dc.Resource(BidGVR), grants: dc.Resource(openbaoctl.GrantGVR), kick: make(chan string, 256),
		busy: map[string]bool{}, exits: map[string]hostsvc.UnitStatus{}, orphans: map[string]time.Time{}, foreign: map[string]bool{},
		logins: map[string]vaultLogin{}, slice: true}, nil
}

func applySets(ctx context.Context, c Config, restCfg *rest.Config) error {
	ap, err := kcpapply.New(restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, Base: c.Workspace, Log: c.log()})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	for _, name := range []string{hostprocess.ProviderSet, hostprocess.ManifestSet, BidderSet} {
		set, ok := ix.Set(name)
		if !ok {
			return fmt.Errorf("computevm: deploy/sets.yaml names no %s set", name)
		}
		values, err := manifests.DecodeValues(set.Values, nil)
		if err != nil {
			return err
		}
		if _, err := ap.Apply(ctx, set, values); err != nil {
			return fmt.Errorf("computevm: applying the %s set: %w", name, err)
		}
	}
	return nil
}

func decode[T any](u *unstructured.Unstructured) (T, error) {
	var o T
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o)
	return o, err
}

func encode(o any, kind string) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(kind)
	return u, nil
}

func ParseMiB(s string) (int, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("computevm: size %q is not <number>[K|M|G|T][i][B]", s)
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

func capMessage(s string) string {
	if len(s) <= MessageCap {
		return s
	}
	return s[:MessageCap/2] + "\n...\n" + s[len(s)-MessageCap/2:]
}

func setCondition(st *v1alpha1.ComputeVMStatus, gen int64, t string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: t, Status: status, Reason: reason, Message: capMessage(msg),
		ObservedGeneration: gen})
}

func (ct *Controller) writeStatus(ctx context.Context, o v1alpha1.ComputeVM, next v1alpha1.ComputeVMStatus) error {
	prev := o.Status
	prev.Market = nil
	next.Market = nil
	next.ObservedGeneration = o.Generation
	next.Message = capMessage(next.Message)
	if reflect.DeepEqual(prev, next) {
		return nil
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&next)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: map[string]any{"status": m}}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("ComputeVM")
	u.SetName(o.Name)
	_, err = ct.vms.ApplyStatus(ctx, o.Name, u, metav1.ApplyOptions{FieldManager: v1alpha1.FieldManagerComputeVMController, Force: true})
	if err == nil && prev.Phase != next.Phase {
		ct.c.log().Info("phase", "computevm", o.Name, "from", string(prev.Phase), "to", string(next.Phase),
			"lostReason", string(next.LostReason), "message", next.Message)
	}
	return err
}

func (ct *Controller) setFinalizer(ctx context.Context, cur *unstructured.Unstructured, res dynamic.ResourceInterface, on bool) error {
	fin := cur.GetFinalizers()
	has := slices.Contains(fin, v1alpha1.FinalizerComputeVM)
	if has == on {
		return nil
	}
	if on {
		fin = append(fin, v1alpha1.FinalizerComputeVM)
	} else {
		fin = slices.DeleteFunc(fin, func(f string) bool { return f == v1alpha1.FinalizerComputeVM })
	}
	cur = cur.DeepCopy()
	cur.SetFinalizers(fin)
	_, err := res.Update(ctx, cur, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (ct *Controller) child(ctx context.Context, name string) (*unstructured.Unstructured, *v1alpha1.HostProcess, error) {
	u, err := ct.hps.Get(ctx, ChildName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	hp, err := decode[v1alpha1.HostProcess](u)
	return u, &hp, err
}

func slotOf(hp *v1alpha1.HostProcess) int {
	if hp == nil {
		return 0
	}
	m := slotRE.FindStringSubmatch(hp.Annotations[hostprocess.AnnotationDescription])
	if m == nil {
		return 0
	}
	k, _ := strconv.Atoi(m[1])
	return k
}

func (ct *Controller) live(ctx context.Context, unit string) (bool, hostsvc.UnitStatus, error) {
	st, err := ct.c.Manager.Inspect(ctx, unit)
	if err != nil {
		return false, st, err
	}
	return st.State == hostsvc.Active || st.State == hostsvc.Activating || st.State == hostsvc.Deactivating || st.State == hostsvc.Reloading, st, nil
}

func (ct *Controller) placer(o v1alpha1.ComputeVM) vmruntime.Placer {
	return func(ctx context.Context, slot int, spec hostsvc.UnitSpec) error {
		ctrl := true
		hp := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: ChildName(o.Name),
			Labels:      map[string]string{LabelComputeVM: o.Name},
			Annotations: map[string]string{hostprocess.AnnotationDescription: spec.Description},
			Finalizers:  []string{v1alpha1.FinalizerComputeVM},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM", Name: o.Name,
				UID: o.UID, Controller: &ctrl}}},
			Spec: hostprocess.SpecOf(spec)}
		u, err := encode(&hp, "HostProcess")
		if err != nil {
			return err
		}
		if _, err := ct.hps.Create(ctx, u, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("computevm: creating the HostProcess child %s: %w", hp.Name, err)
		}
		ct.c.log().Info("HostProcess child created", "computevm", o.Name, "hostprocess", hp.Name, "unit", spec.Name, "slot", slot)
		deadline := time.Now().Add(PlaceTimeout)
		for {
			st, err := ct.c.Manager.Inspect(ctx, spec.Name)
			if err == nil && st.State != hostsvc.NotFound && st.State != "" {
				return nil
			}
			if _, child, cerr := ct.child(ctx, o.Name); cerr == nil && child != nil &&
				(child.Status.Phase == v1alpha1.HostProcessPhaseFailed || child.Status.Phase == v1alpha1.HostProcessPhaseLost) {
				return fmt.Errorf("computevm: the supervisor could not place %s: HostProcess %s is %s %s: %s", spec.Name, child.Name,
					child.Status.Phase, child.Status.LostReason, child.Status.Message)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("computevm: the supervisor did not place %s within %s (last: %v %s)", spec.Name, PlaceTimeout, err, st.State)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
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

func runningStatus(st v1alpha1.ComputeVMStatus, gen int64, ref vmruntime.RuntimeRef) v1alpha1.ComputeVMStatus {
	st.Phase, st.LostReason = v1alpha1.ComputeVMPhaseRunning, ""
	st.Network = &v1alpha1.ComputeVMNetwork{GuestIP: ref.GuestAddress, HostAddress: ref.HostAddress, Prefix: guestnet.Prefix, MAC: guestnet.GuestMAC}
	st.Image = &v1alpha1.ComputeVMImage{Ref: vmruntime.ImageTarget(), Sha256: ref.ImageSHA256, Hypervisor: ref.Hypervisor}
	st.Runtime = &v1alpha1.ComputeVMRuntime{Backend: string(ref.Backend), Ref: ref.Unit, Instance: ref.InvocationID, Pid: int64(ref.PID),
		StartTime: strconv.FormatUint(ref.StartTicks, 10), HostBootID: ref.HostBootID, VMMID: ref.VMMID}
	st.WorkDir = ref.WorkDir
	st.BootedAt = time.Now().UTC().Format(time.RFC3339Nano)
	st.Message = fmt.Sprintf("firecracker %s answers Running in unit %s (pid %d, slot %d)", ref.VMMID, ref.Unit, ref.PID, ref.Slot)
	setCondition(&st, gen, v1alpha1.ConditionStarted, metav1.ConditionTrue, "UnitStarted", ref.Unit)
	setCondition(&st, gen, v1alpha1.ConditionRuntimeObserved, metav1.ConditionTrue, conditionReasonOK, "the unit, the VMM pid and firecracker GET / match the record")
	return st
}

func (ct *Controller) refOf(o v1alpha1.ComputeVM) vmruntime.RuntimeRef {
	h, n := o.Spec.Owner.Handle, o.Name
	if ref, err := ct.c.Runtime.Load(h, n); err == nil {
		return ref
	}
	p := ct.c.Runtime.Placed(h, n)
	ref := vmruntime.RuntimeRef{Handle: h, Name: n, Unit: p.Unit, VMMID: p.VMMID, WorkDir: p.WorkDir, Socket: p.Socket}
	if rt := o.Status.Runtime; rt != nil {
		ticks, _ := strconv.ParseUint(rt.StartTime, 10, 64)
		ref.Backend, ref.InvocationID, ref.PID, ref.StartTicks, ref.HostBootID = hostsvc.Backend(rt.Backend), rt.Instance, int(rt.Pid), ticks, rt.HostBootID
		if rt.VMMID != "" {
			ref.VMMID = rt.VMMID
		}
	}
	return ref
}

func (ct *Controller) fail(ctx context.Context, o v1alpha1.ComputeVM, st v1alpha1.ComputeVMStatus, why error) error {
	p := ct.c.Runtime.Placed(o.Spec.Owner.Handle, o.Name)
	msg := why.Error()
	if t := tail(filepath.Join(p.WorkDir, vmruntime.VMMLogFile), 1500) + tail(filepath.Join(p.WorkDir, vmruntime.ConsoleFile), 1500); t != "" {
		msg += "\n" + t
	}
	ct.c.log().Warn("failed", "computevm", o.Name, "err", why.Error())
	if err := ct.teardown(ctx, o.Name, p.Unit); err != nil {
		ct.c.log().Warn("cleaning up a failed boot", "computevm", o.Name, "err", err.Error())
	}
	st.Phase, st.LostReason, st.Message = v1alpha1.ComputeVMPhaseFailed, "", msg
	return ct.writeStatus(ctx, o, st)
}

func (ct *Controller) boot(ctx context.Context, o v1alpha1.ComputeVM) error {
	rt, h, log := ct.c.Runtime, o.Spec.Owner.Handle, ct.c.log().With("computevm", o.Name)
	st := o.Status
	p := rt.Placed(h, o.Name)
	_, hp, err := ct.child(ctx, o.Name)
	if err != nil {
		return err
	}
	slot := 0
	if hp != nil {
		slot = slotOf(hp)
		log.Info("a HostProcess child exists from before this core; awaiting its VMM", "hostprocess", hp.Name)
	} else {
		if live, _, err := ct.live(ctx, p.Unit); err != nil {
			return err
		} else if live {
			return ct.fail(ctx, o, st, fmt.Errorf("unit %s is running with no HostProcess child and no runtime record; it is not this ComputeVM's VM", p.Unit))
		}
		if _, err := os.Stat(p.WorkDir); err == nil {
			log.Info("removing the work dir of an interrupted prepare", "workDir", p.WorkDir)
			if err := rt.Cleanup(ctx, h, o.Name); err != nil {
				return err
			}
		}
		userData, ok, err := ct.userData(ctx, o)
		if err != nil {
			return ct.fail(ctx, o, st, err)
		}
		if !ok {
			st.Phase, st.Message = v1alpha1.ComputeVMPhasePending, "waiting for the user data at "+o.Spec.UserData.Vault.Server+" "+o.Spec.UserData.Vault.Mount+"/"+o.Spec.UserData.Vault.Path
			return ct.writeStatus(ctx, o, st)
		}
		if err := ct.ensureGuestLogin(ctx, o); err != nil {
			if refusedLogin(err) {
				return ct.fail(ctx, o, st, err)
			}
			st.Phase, st.Message = v1alpha1.ComputeVMPhasePending, "waiting for the guest login role: "+err.Error()
			log.Info("the guest login role is not written yet", "err", err.Error())
			return ct.writeStatus(ctx, o, st)
		}
		mem, err := ParseMiB(o.Spec.VM.Mem)
		if err != nil {
			log.Info("spec.vm.mem does not parse; the guest gets the image's own memory", "mem", o.Spec.VM.Mem)
		}
		disk, err := ParseMiB(o.Spec.VM.Disk)
		if err != nil {
			log.Info("spec.vm.disk does not parse; the guest keeps the image's own disk size", "disk", o.Spec.VM.Disk)
		}
		st.Phase, st.Message = v1alpha1.ComputeVMPhasePreparing, "image ensure, rootfs clone, seed drive"
		if err := ct.writeStatus(ctx, o, st); err != nil {
			return err
		}
		o.Status = st
		started := time.Now()
		prep, err := rt.Prepare(ctx, vmruntime.VMSpec{Handle: h, Name: o.Name, UserData: userData, Vcpu: int(o.Spec.VM.Cpus), MemMiB: mem, DiskMiB: disk})
		if err != nil {
			return ct.fail(ctx, o, st, err)
		}
		var imageMiB int64
		if fi, err := os.Stat(filepath.Join(prep.ImageDir, guestimage.RootfsName)); err == nil {
			imageMiB = fi.Size() >> 20
		}
		log.Info("prepared", "workDir", prep.WorkDir, "vcpu", prep.Vcpu, "memMiB", prep.MemMiB, "diskMiB", disk, "imageRootfsMiB", imageMiB,
			"diskKeptAtImageSize", int64(disk) <= imageMiB, "seconds", time.Since(started).Seconds())
		setCondition(&st, o.Generation, v1alpha1.ConditionPrepared, metav1.ConditionTrue, "Prepared", prep.WorkDir)
		p = prep
		slot, err = rt.Reserve(ctx, p, ct.placer(o))
		if err != nil {
			return ct.fail(ctx, o, st, err)
		}
	}
	st.Phase, st.Message = v1alpha1.ComputeVMPhaseBooting, "unit "+p.Unit+" placed; waiting for firecracker GET / Running"
	setCondition(&st, o.Generation, v1alpha1.ConditionStarted, metav1.ConditionTrue, "UnitStarted", p.Unit)
	if err := ct.writeStatus(ctx, o, st); err != nil {
		return err
	}
	o.Status = st
	ref, err := rt.Await(ctx, p, slot)
	if err != nil {
		return ct.fail(ctx, o, st, err)
	}
	log.Info("running", "unit", ref.Unit, "vmmPid", ref.PID, "slot", ref.Slot, "vmmUid", ref.VMMUID, "invocation", ref.InvocationID)
	return ct.writeStatus(ctx, o, runningStatus(st, o.Generation, ref))
}

func (ct *Controller) observe(ctx context.Context, o v1alpha1.ComputeVM) error {
	st := o.Status
	ref := ct.refOf(o)
	obs, err := ct.c.Runtime.Inspect(ctx, ref)
	if err != nil && obs.Verdict != hostsvc.VerdictLost.String() {
		setCondition(&st, o.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionUnknown, "Unknown", err.Error())
		return ct.writeStatus(ctx, o, st)
	}
	switch obs.Verdict {
	case hostsvc.VerdictRunning.String():
		setCondition(&st, o.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionTrue, conditionReasonOK, "the unit, the VMM pid and firecracker GET / match the record")
		return ct.writeStatus(ctx, o, st)
	case hostsvc.VerdictLost.String():
		ct.mu.Lock()
		exit, seen := ct.exits[o.Name]
		ct.mu.Unlock()
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
		if err := ct.c.Manager.ResetFailed(ctx, ref.Unit); err != nil {
			ct.c.log().Info("reset-failed after a Lost verdict", "computevm", o.Name, "err", err.Error())
		}
		st.Phase, st.LostReason, st.Message = v1alpha1.ComputeVMPhaseLost, reason, msg
		setCondition(&st, o.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionFalse, string(reason), msg)
		ct.c.log().Warn("lost", "computevm", o.Name, "reason", string(reason), "detail", msg)
		return ct.writeStatus(ctx, o, st)
	}
	detail := obs.Detail
	if err != nil {
		detail = err.Error()
	}
	setCondition(&st, o.Generation, v1alpha1.ConditionRuntimeObserved, metav1.ConditionUnknown, "Unknown", detail)
	return ct.writeStatus(ctx, o, st)
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

func (ct *Controller) teardown(ctx context.Context, name, unit string) error {
	cu, hp, err := ct.child(ctx, name)
	if err != nil {
		return err
	}
	if hp != nil && cu.GetDeletionTimestamp() == nil {
		if err := ct.hps.Delete(ctx, ChildName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		ct.c.log().Info("HostProcess child deleted; its supervisor stops the unit", "computevm", name, "unit", unit)
	}
	deadline := time.Now().Add(ReleaseKillAfter)
	for {
		live, _, err := ct.live(ctx, unit)
		if err != nil {
			return err
		}
		if !live {
			break
		}
		if time.Now().After(deadline) {
			ct.c.log().Warn("the unit outlived its HostProcess child; stopping it here", "computevm", name, "unit", unit)
			if err := ct.c.Manager.Stop(ctx, unit); err != nil {
				return err
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	ct.c.Manager.ResetFailed(ctx, unit)
	if cu, hp, err = ct.child(ctx, name); err != nil {
		return err
	} else if hp != nil {
		if err := ct.setFinalizer(ctx, cu, ct.hps, false); err != nil {
			return err
		}
	}
	p := ct.c.Runtime.Placed(ct.c.Handle, name)
	size := dirSize(p.WorkDir)
	if err := ct.c.Runtime.Cleanup(ctx, ct.c.Handle, name); err != nil {
		return err
	}
	for _, d := range []string{p.WorkDir, p.SocketDir} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("computevm: %s is still there after cleanup (%v)", d, err)
		}
	}
	ct.c.log().Info("work dir and API socket removed", "computevm", name, "workDir", p.WorkDir, "bytes", size)
	return nil
}

func (ct *Controller) dropSlice(ctx context.Context) {
	listed, err := ct.c.Runtime.List(ctx, ct.c.Handle)
	if err != nil {
		return
	}
	for _, l := range listed {
		if l.Loaded {
			return
		}
	}
	slice := vmruntime.SliceName(ct.c.Handle)
	if err := ct.c.Manager.StopSlice(ctx, slice); err != nil {
		ct.c.log().Info("stopping the empty per-handle slice", "slice", slice, "err", err.Error())
		return
	}
	ct.mu.Lock()
	ct.slice = false
	ct.mu.Unlock()
	ct.c.log().Info("the per-handle slice is stopped: no VM unit of this handle is loaded", "slice", slice)
}

func (ct *Controller) release(ctx context.Context, cur *unstructured.Unstructured, o v1alpha1.ComputeVM) error {
	if !slices.Contains(o.Finalizers, v1alpha1.FinalizerComputeVM) {
		return nil
	}
	log := ct.c.log().With("computevm", o.Name)
	st := o.Status
	if st.Phase != v1alpha1.ComputeVMPhaseReleasing {
		st.Phase, st.Message = v1alpha1.ComputeVMPhaseReleasing, "graceful stop, then the work dir goes"
		if err := ct.writeStatus(ctx, o, st); err != nil {
			return err
		}
	}
	ref := ct.refOf(o)
	if live, _, err := ct.live(ctx, ref.Unit); err != nil {
		return err
	} else if live {
		grace := ct.c.Runtime.StopGrace()
		if o.Spec.StopGraceSeconds > 0 {
			grace = time.Duration(o.Spec.StopGraceSeconds) * time.Second
		}
		started := time.Now()
		graceful, err := ct.c.Runtime.Shutdown(ctx, ref, grace)
		if err != nil {
			return err
		}
		log.Info("shutdown", "graceful", graceful, "seconds", time.Since(started).Seconds(), "grace", grace.String())
	}
	if err := ct.teardown(ctx, o.Name, ref.Unit); err != nil {
		return err
	}
	ct.dropGuestLogin(ctx, o)
	ct.dropGuestGrants(ctx, o)
	if fresh, err := ct.vms.Get(ctx, o.Name, metav1.GetOptions{}); err == nil {
		cur = fresh
	} else if apierrors.IsNotFound(err) {
		return nil
	}
	if err := ct.setFinalizer(ctx, cur, ct.vms, false); err != nil {
		return err
	}
	ct.mu.Lock()
	delete(ct.exits, o.Name)
	ct.mu.Unlock()
	log.Info("released: the finalizer is dropped")
	ct.dropSlice(ctx)
	return nil
}

func (ct *Controller) reconcile(ctx context.Context, name string) error {
	cur, err := ct.vms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	o, err := decode[v1alpha1.ComputeVM](cur)
	if err != nil {
		return err
	}
	if o.Spec.Owner.Handle != ct.c.Handle {
		ct.mu.Lock()
		seen := ct.foreign[name]
		ct.foreign[name] = true
		ct.mu.Unlock()
		if !seen {
			ct.c.log().Info("a ComputeVM of another handle is left alone", "computevm", name, "owner", o.Spec.Owner.Handle)
		}
		return nil
	}
	if o.DeletionTimestamp != nil {
		return ct.release(ctx, cur, o)
	}
	if !slices.Contains(o.Finalizers, v1alpha1.FinalizerComputeVM) {
		if err := ct.setFinalizer(ctx, cur, ct.vms, true); err != nil {
			return err
		}
		return ct.reconcile(ctx, name)
	}
	ct.mu.Lock()
	ct.slice = true
	ct.mu.Unlock()
	switch o.Status.Phase {
	case v1alpha1.ComputeVMPhaseLost, v1alpha1.ComputeVMPhaseFailed:
		return nil
	case v1alpha1.ComputeVMPhaseRunning:
		return ct.observe(ctx, o)
	}
	if o.Status.Runtime != nil && o.Status.Runtime.Instance != "" {
		return ct.observe(ctx, o)
	}
	return ct.boot(ctx, o)
}

func (ct *Controller) dispatch(ctx context.Context, name string) {
	ct.mu.Lock()
	if ct.busy[name] {
		ct.mu.Unlock()
		return
	}
	ct.busy[name] = true
	ct.mu.Unlock()
	go func() {
		defer func() {
			ct.mu.Lock()
			delete(ct.busy, name)
			ct.mu.Unlock()
		}()
		if err := ct.reconcile(ctx, name); err != nil && !apierrors.IsConflict(err) && ctx.Err() == nil {
			ct.c.log().Warn("reconcile", "computevm", name, "err", err.Error())
		}
	}()
}

func (ct *Controller) collect(ctx context.Context, have map[string]bool) {
	log := ct.c.log()
	listed, err := ct.c.Runtime.List(ctx, ct.c.Handle)
	if err != nil {
		log.Warn("listing VM units and work dirs", "err", err.Error())
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	for _, l := range listed {
		if have[l.Name] {
			continue
		}
		if l.WorkDir == "" {
			ct.mu.Lock()
			known := ct.foreign[l.Unit]
			ct.foreign[l.Unit] = true
			ct.mu.Unlock()
			if !known {
				log.Info("a VM unit of this handle with no work dir under this instance; another instance's, left alone", "unit", l.Unit,
					"state", string(l.Status.State))
			}
			continue
		}
		seen[l.Name] = true
		ct.mu.Lock()
		first, ok := ct.orphans[l.Name]
		if !ok {
			first = now
			ct.orphans[l.Name] = now
		}
		busy := ct.busy[l.Name]
		ct.mu.Unlock()
		if !ok {
			log.Warn("a VM unit or work dir with no ComputeVM of this handle; collected after the grace", "name", l.Name,
				"unit", l.Unit, "loaded", l.Loaded, "state", string(l.Status.State), "workDir", l.WorkDir, "grace", ct.c.grace().String())
		}
		if busy || now.Sub(first) < ct.c.grace() {
			continue
		}
		if l.Loaded {
			if err := ct.c.Manager.Stop(ctx, l.Unit); err != nil {
				log.Warn("stopping an orphan VM unit", "unit", l.Unit, "err", err.Error())
				continue
			}
			log.Warn("orphan VM unit stopped", "unit", l.Unit)
		}
		if err := ct.teardown(ctx, l.Name, l.Unit); err != nil {
			log.Warn("collecting an orphan VM", "name", l.Name, "err", err.Error())
			continue
		}
		ct.mu.Lock()
		delete(ct.orphans, l.Name)
		ct.mu.Unlock()
	}
	ct.mu.Lock()
	for n := range ct.orphans {
		if !seen[n] {
			delete(ct.orphans, n)
		}
	}
	ct.mu.Unlock()
	children, err := ct.hps.List(ctx, metav1.ListOptions{LabelSelector: LabelComputeVM})
	if err == nil {
		for i := range children.Items {
			c := &children.Items[i]
			owner := c.GetLabels()[LabelComputeVM]
			if have[owner] || seen[owner] {
				continue
			}
			if c.GetDeletionTimestamp() == nil {
				ct.hps.Delete(ctx, c.GetName(), metav1.DeleteOptions{})
			}
			if live, _, err := ct.live(ctx, vmruntime.UnitName(ct.c.Handle, owner)); err == nil && !live {
				ct.setFinalizer(ctx, c, ct.hps, false)
				log.Info("a HostProcess child with no ComputeVM is removed", "hostprocess", c.GetName())
			}
		}
	}
	ct.mu.Lock()
	slice := ct.slice
	ct.mu.Unlock()
	if len(have) == 0 && len(seen) == 0 && slice {
		ct.dropSlice(ctx)
	}
}

func (ct *Controller) Pass(ctx context.Context) error {
	list, err := ct.vms.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if h, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "owner", "handle"); h == ct.c.Handle {
			have[name] = true
		}
		ct.dispatch(ctx, name)
	}
	if err := ct.declareGuests(ctx, have); err != nil {
		ct.c.log().Warn("declaring the live ComputeVMs' guest material in the bidder's vault", "err", err.Error())
	}
	ct.collect(ctx, have)
	ct.collectImages(ctx, list.Items)
	ct.linkBids(ctx, list.Items)
	ct.finishBids(ctx, list.Items)
	return nil
}

func (ct *Controller) collectImages(ctx context.Context, vms []unstructured.Unstructured) {
	ct.mu.Lock()
	due := time.Since(ct.imagesAt) >= ImageGCEvery
	if due {
		ct.imagesAt = time.Now()
	}
	ct.mu.Unlock()
	if !due {
		return
	}
	keep := map[string]string{}
	for i := range vms {
		if sha, _, _ := unstructured.NestedString(vms[i].Object, "status", "image", "sha256"); sha != "" {
			keep[sha] = "the ComputeVM " + vms[i].GetName() + " names it"
		}
	}
	rep, err := ct.c.Runtime.CollectImages(ctx, keep)
	log := ct.c.log()
	if err != nil {
		log.Warn("collecting guest images no ComputeVM, versions.json or binary names", "err", err.Error())
		return
	}
	if len(rep.Removed) > 0 {
		log.Info("guest images no ComputeVM, versions.json entry or current or previous binary names are removed",
			"removed", rep.Removed, "kept", len(rep.Kept))
	}
}

func (ct *Controller) watch(ctx context.Context) {
	prefix, suffix := vmruntime.UnitPrefix(ct.c.Handle), hostsvc.ServiceSuffix
	for ctx.Err() == nil {
		ch, err := ct.c.Runtime.Watch(ctx, ct.c.Handle)
		if err != nil {
			ct.c.log().Warn("watching the VM units; retrying", "err", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryAfter):
			}
			continue
		}
		for ev := range ch {
			name := strings.TrimSuffix(strings.TrimPrefix(ev.Name, prefix), suffix)
			if ev.Status.State == hostsvc.Inactive || ev.Status.State == hostsvc.Failed {
				ct.mu.Lock()
				ct.exits[name] = ev.Status
				ct.mu.Unlock()
				ct.c.log().Info("the unit watch saw a VM unit end", "computevm", name, "state", string(ev.Status.State),
					"result", ev.Status.Result, "exitCode", ev.Status.ExecMainStatus)
			}
			select {
			case ct.kick <- name:
			default:
			}
		}
	}
}

func (ct *Controller) Run(ctx context.Context) error {
	interval := ct.c.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	caps := ct.c.Runtime.Capabilities()
	ct.c.log().Info("the ComputeVM controller runs in the core", "workspace", ct.c.Workspace, "backend", string(caps.Backend),
		"hypervisor", caps.Hypervisor, "enforcesLimits", caps.EnforcesLimits, "gracefulStop", caps.GracefulStop)
	go ct.watch(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := ct.Pass(ctx); err != nil {
			ct.c.log().Warn("a reconcile pass failed; VMs keep running", "err", err.Error())
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case name := <-ct.kick:
				ct.dispatch(ctx, name)
			case <-t.C:
				break wait
			}
		}
	}
}

func Supervisor(c Config) hostprocess.Config {
	return hostprocess.Config{Kubeconfig: c.Kubeconfig, Workspace: c.Workspace, Manager: c.Manager, Prefix: vmruntime.UnitPrefix(c.Handle),
		Bound: true, Selector: LabelComputeVM, KeepOrphans: true, Interval: SupervisorEvery, Name: "computevm", Log: c.Log}
}

func Run(ctx context.Context, c Config) {
	log := c.log()
	go hostprocess.Supervise(ctx, Supervisor(c))
	for {
		ct, err := New(ctx, c)
		if err == nil {
			err = ct.Run(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		log.Error("the ComputeVM controller stopped; starting it again", "err", fmt.Sprint(err), "after", RetryAfter.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(RetryAfter):
		}
	}
}

package hostprocess

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/watch"
	"log/slog"
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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/logcap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	ManifestSet      = "hostprocesses"
	ProviderSet      = "provider"
	DefaultWorkspace = "root"
	DefaultInterval  = 5 * time.Second
	RetryAfter       = 2 * time.Second
	MirrorPrefix     = "bootstrap-"
	MirrorLabel      = "socialweb.computer/placement"
	ConditionReady   = "Ready"
	ConditionLimits  = "LimitsEnforced"

	AnnotationDescription = "socialweb.computer/unit-description"
)

var GVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "hostprocesses"}

type Mirror struct {
	Role  string
	Unit  string
	Exec  []string
	Slice string
	After []string
	Wants []string
	Port  int
}

type Config struct {
	Kubeconfig   string
	Workspace    string
	Manager      hostsvc.Manager
	Prefix       string
	Prefixes     []string
	Bound        bool
	Resolve      func(v1alpha1.HostProcessAsset) (string, error)
	Mirrors      func() []Mirror
	Interval     time.Duration
	ApplyTimeout time.Duration
	Selector     string
	KeepOrphans  bool
	Name         string
	Log          *slog.Logger
}

type Supervisor struct {
	c       Config
	client  dynamic.ResourceInterface
	placed  map[string]bool
	listed  bool
	backend hostsvc.Backend

	mu       sync.Mutex
	locks    sync.Map
	placedAt map[string]time.Time
	slices   map[string]bool
}

func (s *Supervisor) setPlaced(unit string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.placed[unit] = true
		if s.placedAt == nil {
			s.placedAt = map[string]time.Time{}
		}
		s.placedAt[unit] = time.Now()
	} else {
		delete(s.placed, unit)
		delete(s.placedAt, unit)
	}
}

func (s *Supervisor) placedAfter(unit string, t time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.placedAt[unit]
	return ok && at.After(t)
}

func (s *Supervisor) isPlaced(unit string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.placed[unit]
}

func (s *Supervisor) lockObject(name string) func() {
	v, _ := s.locks.LoadOrStore(name, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

func (s *Supervisor) markAdopted(o v1alpha1.HostProcess, obs Observed) {
	if o.Spec.Placement == v1alpha1.HostProcessPlacementBootstrap || obs.Inspect != nil || s.isPlaced(o.Spec.Unit) {
		return
	}
	if obs.Unit.State == hostsvc.Active || obs.Unit.State == hostsvc.Activating {
		s.setPlaced(o.Spec.Unit, true)
	}
}

func (s *Supervisor) reconcileObject(ctx context.Context, cur *unstructured.Unstructured) (v1alpha1.HostProcess, bool) {
	unlock := s.lockObject(cur.GetName())
	defer unlock()
	o, err := toObject(cur)
	if err != nil {
		s.c.log().Warn("decoding", "name", cur.GetName(), "err", err.Error())
		return o, false
	}
	s.capOutput(o)
	obs := s.observe(ctx, o)
	s.markAdopted(o, obs)
	next := s.apply(ctx, o, Reconcile(obs))
	if err := s.writeStatus(ctx, cur, next); err != nil && !apierrors.IsConflict(err) {
		s.c.log().Warn("writing status", "name", o.Name, "err", err.Error())
	}
	return o, true
}

func (s *Supervisor) reconcileName(ctx context.Context, name string) {
	cur, err := s.client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			s.c.log().Warn("reading a changed HostProcess", "name", name, "err", err.Error())
		}
		return
	}
	s.reconcileObject(ctx, cur)
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	if c.Name != "" {
		return c.Log.With("component", "hostprocess", "supervisor", c.Name)
	}
	return c.Log.With("component", "hostprocess")
}

func (c Config) prefixes() []string {
	if c.Prefix == "" {
		return c.Prefixes
	}
	return append([]string{c.Prefix}, c.Prefixes...)
}

func (c Config) prefixOf(unit string) string {
	ps := c.prefixes()
	for _, p := range ps {
		if strings.HasPrefix(unit, p) {
			return p
		}
	}
	if len(ps) == 0 {
		return ""
	}
	return ps[0]
}

func (c Config) workspace() string {
	if c.Workspace == "" {
		return DefaultWorkspace
	}
	return c.Workspace
}

func applySets(ctx context.Context, c Config, restCfg *rest.Config) error {
	ap, err := kcpapply.New(restCfg, kcpapply.Options{WaitTimeout: c.ApplyTimeout, Base: c.workspace(), Log: c.log()})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	for _, name := range []string{ProviderSet, ManifestSet} {
		set, ok := ix.Set(name)
		if !ok {
			return fmt.Errorf("hostprocess: deploy/sets.yaml names no %s set", name)
		}
		values, err := manifests.DecodeValues(set.Values, nil)
		if err != nil {
			return err
		}
		if _, err := ap.Apply(ctx, set, values); err != nil {
			return fmt.Errorf("hostprocess: applying the %s set: %w", name, err)
		}
	}
	return nil
}

func New(ctx context.Context, c Config) (*Supervisor, error) {
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if !c.Bound {
		if err := applySets(ctx, c, restCfg); err != nil {
			return nil, err
		}
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + c.workspace()
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Supervisor{c: c, client: dc.Resource(GVR), placed: map[string]bool{}, backend: c.Manager.Capabilities().Backend}, nil
}

func toObject(u *unstructured.Unstructured) (v1alpha1.HostProcess, error) {
	var o v1alpha1.HostProcess
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o)
	return o, err
}

func toUnstructured(o *v1alpha1.HostProcess) (*unstructured.Unstructured, error) {
	o.APIVersion = v1alpha1.GroupVersion.String()
	o.Kind = "HostProcess"
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

func MirrorName(role string) string { return MirrorPrefix + role }

func (s *Supervisor) syncMirrors(ctx context.Context) {
	if s.c.Mirrors == nil {
		return
	}
	for _, m := range s.c.Mirrors() {
		name := MirrorName(m.Role)
		want := v1alpha1.HostProcessSpec{Unit: m.Unit, Placement: v1alpha1.HostProcessPlacementBootstrap, Slice: m.Slice,
			After: m.After, Wants: m.Wants}
		if len(m.Exec) > 0 {
			want.Exec, want.Argv = m.Exec[0], m.Exec[1:]
		}
		cur, err := s.client.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			o := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{MirrorLabel: string(v1alpha1.HostProcessPlacementBootstrap)}}, Spec: want}
			u, err := toUnstructured(&o)
			if err == nil {
				_, err = s.client.Create(ctx, u, metav1.CreateOptions{})
			}
			if err != nil {
				s.c.log().Warn("writing a bootstrap mirror", "name", name, "err", err.Error())
			}
			continue
		}
		if err != nil {
			s.c.log().Warn("reading a bootstrap mirror", "name", name, "err", err.Error())
			continue
		}
		o, err := toObject(cur)
		if err != nil {
			continue
		}
		if o.Spec.Unit != want.Unit || o.Spec.Exec != want.Exec || !slices.Equal(o.Spec.Argv, want.Argv) || o.Spec.Placement != want.Placement {
			o.Spec = want
			if u, err := toUnstructured(&o); err == nil {
				if _, err := s.client.Update(ctx, u, metav1.UpdateOptions{}); err != nil {
					s.c.log().Warn("updating a bootstrap mirror", "name", name, "err", err.Error())
				}
			}
		}
	}
}

func (s *Supervisor) observe(ctx context.Context, o v1alpha1.HostProcess) Observed {
	m := s.c.Manager
	obs := Observed{Object: o, Backend: s.backend, Prefix: s.c.prefixOf(o.Spec.Unit), Deleting: o.DeletionTimestamp != nil}
	if o.Spec.Placement == v1alpha1.HostProcessPlacementBootstrap {
		obs.Prefix = ""
	}
	obs.Unit, obs.Inspect = m.Inspect(ctx, o.Spec.Unit)
	if obs.Inspect != nil {
		return obs
	}
	if o.Spec.Asset != nil && s.c.Resolve != nil {
		if p, err := s.c.Resolve(*o.Spec.Asset); err == nil {
			obs.Exec = p
		}
	}
	rt := o.Status.Runtime
	if rt != nil && rt.InvocationID != "" && rt.Backend == string(s.backend) && o.Spec.Placement != v1alpha1.HostProcessPlacementBootstrap &&
		o.Status.Phase != v1alpha1.HostProcessPhaseLost {
		if RestartedInPlace(o, obs.Unit) {
			obs.Restarted = true
			return obs
		}
		want := hostsvc.Identity{Unit: o.Spec.Unit, InvocationID: rt.InvocationID, PID: int(rt.Pid), StartTicks: uint64(rt.StartTicks),
			BootID: rt.HostBootID, Backend: hostsvc.Backend(rt.Backend), Manager: rt.Manager}
		c, err := m.Check(ctx, want)
		if err != nil {
			obs.Inspect = err
			return obs
		}
		obs.Check = &c
	}
	return obs
}

func setCondition(o *v1alpha1.HostProcess, t string, ok bool, reason, msg string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: t, Status: st, Reason: reason, Message: msg,
		ObservedGeneration: o.Generation})
}

func (s *Supervisor) apply(ctx context.Context, o v1alpha1.HostProcess, r Result) v1alpha1.HostProcess {
	m := s.c.Manager
	log := s.c.log().With("hostprocess", o.Name, "unit", o.Spec.Unit)
	started, stopFailed := false, false
	for _, op := range r.Ops {
		if stopFailed {
			break
		}
		switch op.Kind {
		case OpStart:
			wasPlaced := s.isPlaced(op.Unit)
			s.setPlaced(op.Unit, true)
			st, err := m.Start(ctx, op.Spec)
			if err == nil && st.Adopted && st.State != hostsvc.Active && st.State != hostsvc.Activating {
				log.Info("a loaded unit of this name is not running; resetting it before the start", "state", string(st.State), "result", st.Result)
				if err = m.Stop(ctx, op.Unit); err == nil {
					m.ResetFailed(ctx, op.Unit)
					st, err = m.Start(ctx, op.Spec)
				}
			}
			if err != nil {
				r.Phase, r.Message = v1alpha1.HostProcessPhaseFailed, err.Error()
				log.Warn("start", "err", err.Error())
				if !wasPlaced {
					s.setPlaced(op.Unit, false)
				}
				continue
			}
			s.setPlaced(op.Unit, true)
			started = true
			log.Info("placed", "state", string(st.State), "adopted", st.Adopted, "restartKey", o.Spec.RestartKey)
		case OpStop:
			if err := m.Stop(ctx, op.Unit); err != nil {
				log.Warn("stop", "err", err.Error())
				stopFailed = true
				continue
			}
			s.setPlaced(op.Unit, false)
			log.Info("stopped")
		}
	}
	next := o
	if r.Unknown {
		next.Status.Message = r.Message
		return next
	}
	next.Status.Phase, next.Status.LostReason, next.Status.Message = r.Phase, r.LostReason, r.Message
	next.Status.ObservedGeneration = o.Generation
	if started {
		next.Status.RestartKey = o.Spec.RestartKey
	}
	if r.Identify {
		if id, err := m.Identify(ctx, o.Spec.Unit, 0); err == nil {
			st, _ := m.Inspect(ctx, o.Spec.Unit)
			next.Status.Runtime = &v1alpha1.HostProcessRuntime{Backend: string(id.Backend), Unit: id.Unit, InvocationID: id.InvocationID,
				Pid: int64(id.PID), StartTicks: int64(id.StartTicks), HostBootID: id.BootID, Manager: id.Manager}
			if !st.ActiveSince.IsZero() {
				next.Status.Runtime.ActiveSince = st.ActiveSince.UTC().Format(time.RFC3339Nano)
			}
			if o.Spec.Placement == v1alpha1.HostProcessPlacementSupervisor {
				s.setPlaced(o.Spec.Unit, true)
			}
		}
	}
	if next.Status.Runtime != nil && next.Status.Runtime.Pid > 0 && next.Status.Phase == v1alpha1.HostProcessPhaseRunning {
		next.Status.Listeners = listenersOf(int(next.Status.Runtime.Pid))
	} else {
		next.Status.Listeners = nil
	}
	if r.Phase == v1alpha1.HostProcessPhaseStopped || r.Phase == v1alpha1.HostProcessPhaseFailed || r.Phase == v1alpha1.HostProcessPhaseLost {
		if st, err := m.Inspect(ctx, o.Spec.Unit); err == nil && st.State != hostsvc.NotFound {
			next.Status.LastResult = &v1alpha1.HostProcessResult{Result: st.Result, ExitCode: st.ExecMainStatus,
				At: time.Now().UTC().Format(time.RFC3339)}
		}
	}
	setCondition(&next, ConditionReady, next.Status.Phase == v1alpha1.HostProcessPhaseRunning, string(next.Status.Phase), next.Status.Message)
	caps := m.Capabilities()
	setCondition(&next, ConditionLimits, caps.EnforcesLimits, string(caps.Backend), "limits are enforced only where the backend has cgroups")
	return next
}

func (s *Supervisor) writeStatus(ctx context.Context, cur *unstructured.Unstructured, next v1alpha1.HostProcess) error {
	u, err := toUnstructured(&next)
	if err != nil {
		return err
	}
	if equalStatus(cur.Object["status"], u.Object["status"]) {
		return nil
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	_, err = s.client.UpdateStatus(ctx, u, metav1.UpdateOptions{})
	return err
}

func equalStatus(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func (s *Supervisor) Pass(ctx context.Context) error {
	s.syncMirrors(ctx)
	listedAt := time.Now()
	list, err := s.client.List(ctx, metav1.ListOptions{LabelSelector: s.c.Selector})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var liveSlices []string
	for i := range list.Items {
		if o, ok := s.reconcileObject(ctx, &list.Items[i]); ok {
			seen[o.Spec.Unit] = true
			liveSlices = append(liveSlices, o.Spec.Slice)
		}
	}
	for _, prefix := range s.c.prefixes() {
		if s.c.KeepOrphans {
			break
		}
		units, err := s.c.Manager.List(ctx, prefix)
		if err == nil {
			for _, u := range units {
				if !seen[u.Name] && u.State == hostsvc.Failed && !s.placedAfter(u.Name, listedAt) {
					s.c.log().Info("the HostProcess of a failed unit is gone; resetting the unit", "unit", u.Name)
					if err := s.c.Manager.ResetFailed(ctx, u.Name); err != nil {
						s.c.log().Warn("resetting the failed unit of a gone HostProcess failed; the next pass retries", "unit", u.Name, "err", err.Error())
					} else {
						s.setPlaced(u.Name, false)
					}
					continue
				}
				if !s.isPlaced(u.Name) && s.listed {
					continue
				}
				if !seen[u.Name] && s.placedAfter(u.Name, listedAt) {
					continue
				}
				if !seen[u.Name] && (u.State == hostsvc.Active || u.State == hostsvc.Activating) {
					s.c.log().Info("the HostProcess of a placed unit is gone; stopping the unit", "unit", u.Name)
					if err := s.c.Manager.Stop(ctx, u.Name); err != nil {
						s.c.log().Warn("stopping the unit of a gone HostProcess failed; the next pass retries", "unit", u.Name, "err", err.Error())
					} else {
						s.setPlaced(u.Name, false)
					}
				}
			}
		}
	}
	s.listed = true
	if !s.c.KeepOrphans {
		s.sweepSlices(ctx, liveSlices)
	}
	return nil
}

const ClusterSlicePart = "cluster-"

func (s *Supervisor) sweepable(slice string) bool {
	if hostsvc.ValidateSlice(slice) != nil {
		return false
	}
	for _, p := range s.c.prefixes() {
		if strings.HasPrefix(slice, p+ClusterSlicePart) {
			return true
		}
	}
	return false
}

func (s *Supervisor) sweepSlices(ctx context.Context, live []string) {
	s.mu.Lock()
	if s.slices == nil {
		s.slices = map[string]bool{}
	}
	for _, sl := range live {
		if s.sweepable(sl) {
			s.slices[sl] = true
		}
	}
	var empty []string
	for sl := range s.slices {
		held := false
		for _, l := range live {
			if hostsvc.SliceContains(sl, l) {
				held = true
				break
			}
		}
		if !held {
			empty = append(empty, sl)
		}
	}
	s.mu.Unlock()
	for _, sl := range empty {
		if err := s.c.Manager.StopSlice(ctx, sl); err != nil {
			s.c.log().Warn("stopping the empty slice of gone HostProcesses failed; the next pass retries", "slice", sl, "err", err.Error())
			continue
		}
		s.mu.Lock()
		delete(s.slices, sl)
		s.mu.Unlock()
		s.c.log().Info("every HostProcess of the slice is gone; the empty slice is stopped", "slice", sl)
	}
}

func (s *Supervisor) capOutput(o v1alpha1.HostProcess) {
	out := o.Spec.Output
	if out == nil {
		return
	}
	capBytes := out.CapBytes
	if capBytes == 0 {
		capBytes = logcap.DefaultCap
	}
	files := []string{out.Stdout}
	if out.Stderr != out.Stdout {
		files = append(files, out.Stderr)
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		ok, err := logcap.Rotate(f, capBytes)
		if err != nil {
			s.c.log().Warn("an output file over its cap was not rotated", "name", o.Name, "file", f, "err", err.Error())
		}
		if ok {
			s.c.log().Info("an output file over its cap was rotated; it keeps its newest bytes in a .1 copy", "name", o.Name, "file", f, "cap", capBytes)
		}
	}
}

func (s *Supervisor) Run(ctx context.Context) error {
	interval := s.c.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	s.c.log().Info("the HostProcess supervisor runs in the core", "workspace", s.c.workspace(), "backend", string(s.backend),
		"prefixes", s.c.prefixes())
	t := time.NewTicker(interval)
	defer t.Stop()
	wake := make(chan struct{}, 1)
	changed := make(chan string, 1024)
	for i := 0; i < EventWorkers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case name := <-changed:
					s.reconcileName(ctx, name)
				}
			}
		}()
	}
	go s.watch(ctx, wake, changed)
	for {
		if err := s.Pass(ctx); err != nil {
			s.c.log().Warn("a reconcile pass failed; units keep running", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-wake:
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(WakeDebounce):
			}
			select {
			case <-wake:
			default:
			}
		}
	}
}

const WakeDebounce = 100 * time.Millisecond

const EventWorkers = 4

func (s *Supervisor) watch(ctx context.Context, wake chan<- struct{}, changedCh chan<- string) {
	generations := map[string]int64{}
	failing := false
	for ctx.Err() == nil {
		w, err := s.client.Watch(ctx, metav1.ListOptions{LabelSelector: s.c.Selector})
		if err != nil {
			if !failing {
				s.c.log().Warn("watching HostProcess objects failed; the supervisor keeps its interval and retries the watch", "err", err.Error())
				failing = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryAfter):
			}
			continue
		}
		if failing {
			s.c.log().Info("watching HostProcess objects again")
			failing = false
		}
		for ev := range w.ResultChan() {
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			name, gen := u.GetName(), u.GetGeneration()
			changed := ev.Type != watch.Modified || generations[name] != gen
			changedNames := changedCh
			if ev.Type == watch.Deleted {
				delete(generations, name)
			} else {
				generations[name] = gen
			}
			if !changed {
				continue
			}
			if ev.Type != watch.Deleted {
				select {
				case changedNames <- name:
					continue
				default:
				}
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		w.Stop()
	}
}

func Supervise(ctx context.Context, c Config) {
	log := c.log()
	for {
		s, err := New(ctx, c)
		if err == nil {
			err = s.Run(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		log.Error("the HostProcess supervisor stopped; starting it again", "err", fmt.Sprint(err), "after", RetryAfter.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(RetryAfter):
		}
	}
}

func listenersOf(pid int) []v1alpha1.HostProcessListener {
	ports, err := hostsvc.ListenPorts(pid)
	if err != nil || len(ports) == 0 {
		return nil
	}
	if len(ports) == 1 {
		return []v1alpha1.HostProcessListener{{Name: "tcp", Address: "127.0.0.1:" + strconv.Itoa(ports[0])}}
	}
	out := make([]v1alpha1.HostProcessListener, 0, len(ports))
	for _, p := range ports {
		out = append(out, v1alpha1.HostProcessListener{Name: "tcp-" + strconv.Itoa(p), Address: "127.0.0.1:" + strconv.Itoa(p)})
	}
	return out
}

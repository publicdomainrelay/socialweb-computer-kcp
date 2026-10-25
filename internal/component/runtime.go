package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultRestartBackoff    = time.Second
	DefaultMaxRestartBackoff = 30 * time.Second
	DefaultStopTimeout       = 30 * time.Second
	ReadyTimeout             = 2 * time.Minute
	DetachedReadyGrace       = 5 * time.Second
	StatusWriteTimeout       = 10 * time.Second
)

var (
	ErrRuntimeStarted    = errors.New("component: the runtime is already started")
	ErrRuntimeStopped    = errors.New("component: the runtime is stopped")
	ErrRuntimeNotStarted = errors.New("component: the runtime is not started")
	ErrNoFactory         = errors.New("component: no service factory is registered for kind")
)

type ServiceFactory func(ctx context.Context, in Instance, spec ComponentSpec) (Service, error)

func Typed[S any, T Service](build func(ctx context.Context, in Instance, spec S) (T, error)) ServiceFactory {
	return func(ctx context.Context, in Instance, raw ComponentSpec) (Service, error) {
		var spec S
		if err := raw.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("component: decoding the %s spec: %w", raw.Kind, err)
		}
		svc, err := build(ctx, in, spec)
		if err != nil {
			return nil, err
		}
		return svc, nil
	}
}

type RuntimeConfig struct {
	Log      *slog.Logger
	StateDir string
	Registry *Registry
	Bus      *bus.Bus
}

type StatusCallback func(ctx context.Context, spec ComponentSpec, status ComponentStatus) error

type PanicError struct {
	Component string
	Value     any
	Stack     []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("component: %s panicked: %v", e.Component, e.Value)
}

func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

type Runtime struct {
	log      *slog.Logger
	stateDir string
	registry *Registry
	bus      *bus.Bus

	makeStateDir func(dir string) error

	mu        sync.Mutex
	instances map[types.UID]*instance
	factories map[string]ServiceFactory
	root      context.Context
	cancel    context.CancelFunc
	started   bool
	closed    bool
	changed   chan struct{}

	callbackMu sync.Mutex
	onStatus   StatusCallback

	restartBackoff    time.Duration
	maxRestartBackoff time.Duration
	readyTimeout      time.Duration
}

type instance struct {
	opMu sync.Mutex
	mu   sync.Mutex

	uid         types.UID
	key         Key
	spec        ComponentSpec
	svc         Service
	in          Instance
	cancel      context.CancelFunc
	done        chan struct{}
	attempt     uint64
	restarts    int32
	status      ComponentStatus
	startedAt   time.Time
	running     bool
	started     bool
	ended       bool
	stopping    bool
	stopClaimed bool

	supervising bool
}

func NewRuntime(cfg RuntimeConfig, onStatus StatusCallback) *Runtime {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Registry == nil {
		cfg.Registry = NewRegistry()
	}
	return &Runtime{
		log:               cfg.Log,
		stateDir:          cfg.StateDir,
		registry:          cfg.Registry,
		bus:               cfg.Bus,
		instances:         map[types.UID]*instance{},
		factories:         map[string]ServiceFactory{},
		onStatus:          onStatus,
		restartBackoff:    DefaultRestartBackoff,
		maxRestartBackoff: DefaultMaxRestartBackoff,
		readyTimeout:      ReadyTimeout,
	}
}

func (r *Runtime) SetStatusCallback(cb StatusCallback) {
	r.callbackMu.Lock()
	r.onStatus = cb
	r.callbackMu.Unlock()
}

func (r *Runtime) SetStateDirMaker(fn func(dir string) error) {
	r.mu.Lock()
	r.makeStateDir = fn
	r.mu.Unlock()
}

func (r *Runtime) SetBackoff(initial, max time.Duration) error {
	switch {
	case initial <= 0:
		return errors.New("component: the restart backoff must be positive")
	case max < initial:
		return errors.New("component: the maximum restart backoff is below the initial backoff")
	}
	r.mu.Lock()
	r.restartBackoff = initial
	r.maxRestartBackoff = max
	r.mu.Unlock()
	return nil
}

func (r *Runtime) prepareStateDir(dir string) error {
	if r.makeStateDir == nil {
		return nil
	}
	return r.makeStateDir(dir)
}

func (r *Runtime) Registry() *Registry {
	return r.registry
}

func (r *Runtime) Bus() *bus.Bus {
	return r.bus
}

func (r *Runtime) Register(kind string, factory ServiceFactory) error {
	switch {
	case kind == "":
		return errors.New("component: a service factory needs a kind")
	case factory == nil:
		return fmt.Errorf("component: the service factory for %q is nil", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRuntimeStopped
	}
	r.factories[kind] = factory
	return nil
}

func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.closed:
		return ErrRuntimeStopped
	case r.started:
		return ErrRuntimeStarted
	}
	r.root, r.cancel = context.WithCancel(ctx)
	r.started = true
	return nil
}

func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started && !r.closed {
		r.mu.Unlock()
		return nil
	}
	r.started = false
	r.closed = true
	cancel := r.cancel
	instances := make([]*instance, 0, len(r.instances))
	for _, inst := range r.instances {
		instances = append(instances, inst)
	}
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	stopCtx, stopCancel := stopContext(ctx)
	defer stopCancel()
	var errs []error
	for _, inst := range instances {
		inst.opMu.Lock()
		if err := r.stopInstanceLocked(stopCtx, inst); err != nil {
			errs = append(errs, err)
		}
		r.forget(inst)
		inst.opMu.Unlock()
	}
	if r.bus != nil {
		if err := r.bus.Stop(stopCtx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Runtime) Reconcile(ctx context.Context, spec ComponentSpec) (ComponentStatus, error) {
	if err := spec.Check(); err != nil {
		st := statusFromSpec(spec)
		st.Phase = PhaseFailed
		st.Message = err.Error()
		conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "InvalidSpec", err.Error())
		r.emit(ctx, spec, st)
		return st, err
	}
	inst, err := r.instanceFor(spec)
	if err != nil {
		return statusFromSpec(spec), err
	}
	inst.opMu.Lock()
	defer inst.opMu.Unlock()

	inst.mu.Lock()
	running := inst.running
	supervising := inst.supervising
	changed := inst.spec.Generation != spec.Generation
	attempted := inst.started
	current := cloneStatus(inst.status)
	restarts := inst.restarts
	inst.mu.Unlock()

	if spec.Deleting {
		stopErr := r.stopInstanceLocked(ctx, inst)
		r.forget(inst)
		st := statusFromSpec(spec)
		st.Phase = PhaseStopping
		st.Message = "the component was deleted"
		st.Restarts = restarts
		conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "Deleted", st.Message)
		r.emit(ctx, spec, st)
		return st, stopErr
	}
	if spec.Suspend {
		stopErr := r.stopInstanceLocked(ctx, inst)
		st := statusFromSpec(spec)
		st.Phase = PhaseSuspended
		st.Message = "the component is suspended"
		st.Restarts = restarts
		conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "Suspended", st.Message)
		inst.mu.Lock()
		inst.spec, inst.status = spec, cloneStatus(st)
		inst.mu.Unlock()
		r.emit(ctx, spec, st)
		return st, stopErr
	}
	if !changed && attempted {
		return current, nil
	}
	if running || supervising {
		if err := r.stopInstanceLocked(ctx, inst); err != nil {
			st := failedStatus(spec, restarts, err)
			r.emit(ctx, spec, st)
			return st, err
		}
	}
	st, err := r.startLocked(ctx, inst, spec)
	return st, err
}

func (r *Runtime) Remove(ctx context.Context, uid types.UID) error {
	r.mu.Lock()
	inst := r.instances[uid]
	r.mu.Unlock()
	if inst == nil {
		return nil
	}
	inst.opMu.Lock()
	defer inst.opMu.Unlock()
	err := r.stopInstanceLocked(ctx, inst)
	r.forget(inst)
	return err
}

func (r *Runtime) Status(uid types.UID) (ComponentStatus, bool) {
	r.mu.Lock()
	inst := r.instances[uid]
	r.mu.Unlock()
	if inst == nil {
		return ComponentStatus{}, false
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return cloneStatus(inst.status), true
}

func (r *Runtime) instanceFor(spec ComponentSpec) (*instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || r.closed {
		return nil, ErrRuntimeNotStarted
	}
	inst := r.instances[spec.UID]
	if inst == nil {
		inst = &instance{
			uid: spec.UID,
			key: Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name},
		}
		r.instances[spec.UID] = inst
	}
	return inst, nil
}

func (r *Runtime) forget(inst *instance) {
	r.mu.Lock()
	if r.instances[inst.uid] == inst {
		delete(r.instances, inst.uid)
	}
	r.mu.Unlock()
	r.broadcast()
}

func (r *Runtime) Await(ctx context.Context, key Key, serves func(Service) bool) (Service, bool) {
	if r == nil || r.registry == nil {
		return nil, false
	}
	timer := time.NewTimer(r.readyTimeoutOf())
	defer timer.Stop()
	for {
		changed := r.changes()
		if svc, ok := r.registry.Service(key.Workspace, key.Kind, key.Name); ok && (serves == nil || serves(svc)) {
			return svc, true
		}
		if !r.pending(key) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-timer.C:
			return nil, false
		case <-changed:
		}
	}
}

func (r *Runtime) pending(key Key) bool {
	r.mu.Lock()
	insts := make([]*instance, 0, len(r.instances))
	for _, inst := range r.instances {
		insts = append(insts, inst)
	}
	r.mu.Unlock()
	for _, inst := range insts {
		inst.mu.Lock()
		coming := inst.key == key && !inst.spec.Suspend && !inst.spec.Deleting && inst.status.Phase != PhaseFailed
		inst.mu.Unlock()
		if coming {
			return true
		}
	}
	return false
}

func (r *Runtime) changes() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changed == nil {
		r.changed = make(chan struct{})
	}
	return r.changed
}

func (r *Runtime) broadcast() {
	r.mu.Lock()
	changed := r.changed
	r.changed = nil
	r.mu.Unlock()
	if changed != nil {
		close(changed)
	}
}

func (r *Runtime) startLocked(ctx context.Context, inst *instance, spec ComponentSpec) (ComponentStatus, error) {
	factory, err := r.factoryFor(spec.Kind)
	if err != nil {
		return r.failLocked(ctx, inst, spec, err)
	}
	root, err := r.rootContext()
	if err != nil {
		return r.failLocked(ctx, inst, spec, err)
	}
	in := r.instanceOf(spec)
	if in.StateDir == "" {
		return r.failLocked(ctx, inst, spec, fmt.Errorf("component: %s/%s has no state dir", spec.Kind, spec.Name))
	}
	if err := r.prepareStateDir(in.StateDir); err != nil {
		return r.failLocked(ctx, inst, spec, fmt.Errorf("component: creating the state dir for %s/%s: %w", spec.Kind, spec.Name, err))
	}
	svc, buildErr := r.callFactory(ctx, spec, factory, in)
	switch {
	case buildErr != nil:
		svc, buildErr = nil, fmt.Errorf("component: building %s/%s: %w", spec.Kind, spec.Name, buildErr)
	case svc == nil:
		buildErr = fmt.Errorf("component: the factory for %s/%s returned no service", spec.Kind, spec.Name)
	}
	runCtx, cancel := context.WithCancel(specCaller(root, spec))
	done := make(chan struct{})
	startedAt := time.Now().UTC()
	inst.mu.Lock()
	restarts := inst.restarts
	inst.key = Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name}
	inst.spec = spec
	inst.svc = svc
	inst.in = in
	inst.cancel = cancel
	inst.done = done
	inst.startedAt = startedAt
	inst.attempt++
	attempt := inst.attempt
	inst.status = startingStatus(spec, restarts)
	inst.status.StartedAt = startedAt
	st := inst.status
	inst.running = buildErr == nil
	inst.started = true
	inst.ended = false
	inst.stopping = false
	inst.stopClaimed = false
	inst.supervising = true
	inst.mu.Unlock()

	if buildErr == nil {
		r.emit(ctx, spec, st)
	}
	go r.supervise(inst, spec, in, svc, buildErr, runCtx, done, attempt)
	return st, nil
}

func (r *Runtime) failLocked(ctx context.Context, inst *instance, spec ComponentSpec, err error) (ComponentStatus, error) {
	inst.mu.Lock()
	st := failedStatus(spec, inst.restarts, err)
	inst.spec, inst.status, inst.started = spec, cloneStatus(st), true
	inst.mu.Unlock()
	r.emit(ctx, spec, st)
	return st, err
}

func (r *Runtime) stopInstanceLocked(ctx context.Context, inst *instance) error {
	inst.mu.Lock()
	svc := inst.svc
	cancel := inst.cancel
	done := inst.done
	description := fmt.Sprintf("%s/%s", inst.key.Kind, inst.key.Name)
	key := inst.key
	if svc == nil && done == nil {
		inst.mu.Unlock()
		return nil
	}
	callStop := svc != nil && !inst.ended && !inst.stopClaimed
	if callStop {
		inst.stopClaimed = true
	}
	inst.stopping = true
	uid := inst.uid
	inst.mu.Unlock()

	if r.registry != nil && r.registry.Unregister(key, uid) {
		r.broadcast()
	}
	if cancel != nil {
		cancel()
	}
	var stopErr error
	if callStop {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultStopTimeout)
		stopErr = r.callStop(stopCtx, description, key, svc)
		stopCancel()
		if stopErr != nil {
			stopErr = fmt.Errorf("component: stopping %s: %w", description, stopErr)
		}
	}
	if done != nil {
		waitCtx, waitCancel := stopContext(ctx)
		select {
		case <-done:
		case <-waitCtx.Done():
			if stopErr == nil {
				stopErr = fmt.Errorf("component: stopping %s: %w", description, waitCtx.Err())
			}
		}
		waitCancel()
	}
	r.unregister(inst)
	inst.mu.Lock()
	inst.svc = nil
	inst.cancel = nil
	inst.done = nil
	inst.running = false
	inst.started = false
	inst.ended = false
	inst.stopping = false
	inst.supervising = false
	inst.mu.Unlock()
	return stopErr
}

func (r *Runtime) supervise(inst *instance, spec ComponentSpec, in Instance, svc Service, buildErr error, runCtx context.Context, done chan struct{}, attempt uint64) {
	defer func() {
		inst.mu.Lock()
		inst.ended = true
		if inst.attempt == attempt {
			inst.running = false
			inst.supervising = false
		}
		inst.mu.Unlock()
		close(done)
	}()
	var probes sync.WaitGroup
	defer probes.Wait()
	restarts := inst.restartsOf()
	for {
		if svc == nil {
			var next Service
			ferr := buildErr
			buildErr = nil
			if ferr == nil {
				next, ferr = r.buildService(runCtx, spec, in)
			}
			if ferr != nil {
				restarts++
				r.log.Warn("component: building the service failed", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace,
					"restarts", restarts, "retryIn", r.backoffFor(restarts).String(), "err", ferr)
				st := failedStatus(spec, restarts, ferr)
				st.StartedAt = inst.startedAtOf()
				inst.updateStatus(attempt, st, restarts, false)
				r.emit(context.WithoutCancel(runCtx), spec, st)
				if !restartAllowed(spec.RestartPolicy, ferr) || !r.waitRestart(runCtx, restarts) {
					return
				}
				continue
			}
			if runCtx.Err() != nil || inst.isStopping() {
				return
			}
			startedAt := time.Now().UTC()
			inst.mu.Lock()
			if inst.stopping {
				inst.mu.Unlock()
				return
			}
			inst.attempt++
			attempt = inst.attempt
			st := startingStatus(spec, restarts)
			st.StartedAt = startedAt
			inst.svc = next
			inst.startedAt = startedAt
			inst.status = st
			inst.running = true
			inst.stopClaimed = false
			inst.supervising = true
			inst.mu.Unlock()
			r.emit(context.WithoutCancel(runCtx), spec, st)
			svc = next
		}

		if runCtx.Err() != nil || inst.isStopping() {
			inst.deactivate(attempt)
			r.unregister(inst)
			return
		}
		started := make(chan struct{})
		probeCtx, probeCancel := context.WithCancel(runCtx)
		ready := r.launchReady(&probes, inst, svc, probeCtx, attempt, started)
		close(started)
		err := r.callStart(runCtx, svc, spec)
		if runCtx.Err() != nil || inst.isStopping() {
			probeCancel()
			inst.deactivate(attempt)
			r.unregister(inst)
			return
		}
		if err == nil {
			if r.awaitDetached(runCtx, inst, attempt, ready) {
				probeCancel()
				inst.deactivate(attempt)
				r.unregister(inst)
				return
			}
		}
		probeCancel()
		inst.deactivate(attempt)
		r.unregister(inst)
		if inst.claimStop() {
			stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(runCtx), DefaultStopTimeout)
			if stopErr := r.callStop(stopCtx, spec.Kind+"/"+spec.Name, Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name}, svc); stopErr != nil {
				r.log.Warn("component: stopping the exited service failed", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace, "err", stopErr)
			}
			stopCancel()
		}
		if runCtx.Err() != nil || inst.isStopping() {
			return
		}
		restarts++
		if err != nil {
			r.log.Warn("component: service exited", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace, "restarts", restarts, "err", err)
			var panicErr *PanicError
			if errors.As(err, &panicErr) && len(panicErr.Stack) > 0 {
				r.log.Error("component: service panicked", "component", spec.Kind+"/"+spec.Name, "stack", string(panicErr.Stack))
			}
		}
		st := statusFromSpec(spec)
		st.Phase = PhaseFailed
		st.Restarts = restarts
		st.StartedAt = inst.startedAtOf()
		if err != nil {
			st.Message = err.Error()
		} else {
			st.Message = "the service exited"
		}
		conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "ServiceExited", st.Message)
		inst.updateStatus(attempt, st, restarts, false)
		r.emit(context.WithoutCancel(runCtx), spec, st)
		if !restartAllowed(spec.RestartPolicy, err) || !r.waitRestart(runCtx, restarts) {
			return
		}
		svc = nil
	}
}

func (r *Runtime) buildService(ctx context.Context, spec ComponentSpec, in Instance) (Service, error) {
	factory, err := r.factoryFor(spec.Kind)
	if err != nil {
		return nil, err
	}
	svc, err := r.callFactory(ctx, spec, factory, in)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("component: the factory for %s/%s returned no service", spec.Kind, spec.Name)
	}
	return svc, nil
}

func (r *Runtime) waitRestart(ctx context.Context, restarts int32) bool {
	timer := time.NewTimer(r.backoffFor(restarts))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Runtime) launchReady(probes *sync.WaitGroup, inst *instance, svc Service, ctx context.Context, attempt uint64, started <-chan struct{}) chan struct{} {
	ready := make(chan struct{})
	probes.Go(func() { r.awaitReady(inst, svc, ctx, attempt, started, ready) })
	return ready
}

func (r *Runtime) awaitDetached(ctx context.Context, inst *instance, attempt uint64, ready <-chan struct{}) bool {
	timer := time.NewTimer(DetachedReadyGrace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-ready:
		<-ctx.Done()
		return true
	case <-timer.C:
		if inst.isReady(attempt) {
			<-ctx.Done()
			return true
		}
		return false
	}
}

func (r *Runtime) awaitReady(inst *instance, svc Service, ctx context.Context, attempt uint64, started <-chan struct{}, ready chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-started:
	}
	signal, done, err := r.callReady(inst, svc)
	if err != nil {
		var panicErr *PanicError
		if errors.As(err, &panicErr) {
			r.log.Error("component: service ready check panicked", "component", panicErr.Component, "stack", string(panicErr.Stack))
			return
		}
		r.notReady(ctx, inst, svc, attempt, fmt.Sprintf("the service is not ready: %v", err))
		return
	}
	timeout := r.readyTimeoutOf()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-done:
	case <-timer.C:
		r.notReady(ctx, inst, svc, attempt, fmt.Sprintf("the service is not ready after %s", timeout))
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
	}
	if ctx.Err() != nil {
		return
	}
	if err := signal.Err(); err != nil {
		r.notReady(ctx, inst, svc, attempt, fmt.Sprintf("the service is not ready: %v", err))
		return
	}
	endpoints, err := r.callEndpoints(inst, svc)
	if err != nil {
		r.log.Error("component: reading service endpoints failed", "err", err)
		return
	}
	inst.mu.Lock()
	if inst.attempt != attempt || !inst.running || inst.svc != svc || inst.stopping {
		inst.mu.Unlock()
		return
	}
	spec := inst.spec
	restarts := inst.restarts
	st := startingStatus(spec, restarts)
	st.StartedAt = inst.startedAt
	st.Phase = PhaseReady
	st.Ready = true
	st.Endpoints = cloneEndpoints(endpoints)
	st.Message = ""
	conds.Set(&st.Conditions, spec.Generation, ConditionReady, true, "Ready", "the service reports ready")
	inst.status = st
	inst.mu.Unlock()
	if !r.register(inst, svc, endpoints) {
		return
	}
	r.emit(context.WithoutCancel(ctx), spec, st)
	close(ready)
}

func (r *Runtime) notReady(ctx context.Context, inst *instance, svc Service, attempt uint64, message string) {
	spec := inst.specSnapshot()
	restarts := inst.restartsOf()
	st := startingStatus(spec, restarts)
	st.StartedAt = inst.startedAtOf()
	st.Message = message
	conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "NotReady", st.Message)
	inst.mu.Lock()
	updated := inst.attempt == attempt && inst.running && inst.svc == svc && !inst.stopping
	if updated {
		inst.status = st
	}
	inst.mu.Unlock()
	if updated {
		r.emit(context.WithoutCancel(ctx), spec, st)
	}
}

func (r *Runtime) register(inst *instance, svc Service, endpoints []Endpoint) bool {
	if r.registry == nil {
		return false
	}
	inst.mu.Lock()
	if inst.svc != svc || !inst.running || inst.stopping {
		inst.mu.Unlock()
		return false
	}
	entry := Entry{
		Key:        inst.key,
		UID:        inst.uid,
		Generation: inst.spec.Generation,
		Endpoints:  cloneEndpoints(endpoints),
		Service:    inst.svc,
	}
	inst.mu.Unlock()
	r.registry.Put(entry)
	r.broadcast()
	return true
}

func (r *Runtime) unregister(inst *instance) {
	if r.registry == nil {
		return
	}
	inst.mu.Lock()
	key := inst.key
	uid := inst.uid
	inst.mu.Unlock()
	r.registry.Unregister(key, uid)
	if r.bus != nil {
		r.bus.Remove(key)
	}
	r.broadcast()
}

func (r *Runtime) emit(ctx context.Context, spec ComponentSpec, status ComponentStatus) {
	r.broadcast()
	defer func() {
		if value := recover(); value != nil {
			r.log.Error("component: status callback panicked",
				"component", spec.Kind+"/"+spec.Name,
				"workspace", spec.Workspace,
				"value", value,
				"stack", string(debug.Stack()))
		}
	}()
	r.callbackMu.Lock()
	cb := r.onStatus
	r.callbackMu.Unlock()
	if cb == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatusWriteTimeout)
	defer cancel()
	if err := cb(writeCtx, spec, status); err != nil {
		r.log.Warn("component: status callback failed", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace, "err", err)
	}
}

func (r *Runtime) callStart(ctx context.Context, svc Service, spec ComponentSpec) (err error) {
	name := spec.Kind + "/" + spec.Name
	defer func() {
		if value := recover(); value != nil {
			err = newPanicError(name, value)
		}
	}()
	return svc.Start(specCaller(ctx, spec))
}

func (r *Runtime) callReady(inst *instance, svc Service) (signal Readiness, done <-chan struct{}, err error) {
	name := inst.name()
	defer func() {
		if value := recover(); value != nil {
			signal, done = nil, nil
			err = newPanicError(name, value)
		}
	}()
	signal = svc.Ready()
	if signal == nil {
		return nil, nil, fmt.Errorf("component: %s has no readiness signal", name)
	}
	return signal, signal.Done(), nil
}

func (r *Runtime) callEndpoints(inst *instance, svc Service) (endpoints []Endpoint, err error) {
	name := inst.name()
	defer func() {
		if value := recover(); value != nil {
			endpoints = nil
			err = newPanicError(name, value)
		}
	}()
	return svc.Endpoints(), nil
}

func (r *Runtime) callStop(ctx context.Context, name string, key Key, svc Service) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = newPanicError(name, value)
		}
	}()
	return svc.Stop(WithCaller(ctx, key))
}

func (r *Runtime) callFactory(ctx context.Context, spec ComponentSpec, factory ServiceFactory, in Instance) (svc Service, err error) {
	name := spec.Kind + "/" + spec.Name
	defer func() {
		if value := recover(); value != nil {
			svc = nil
			err = newPanicError(name, value)
		}
	}()
	return factory(specCaller(ctx, spec), in, spec)
}

func (r *Runtime) instanceOf(spec ComponentSpec) Instance {
	return Instance{
		Self:      spec.Self(),
		UID:       spec.UID,
		Namespace: spec.Namespace,
		Labels:    spec.Labels,
		Log:       r.log,
		StateDir:  spec.ComponentStateDir(r.stateDir),
		Bus:       r.bus,
	}
}

func specCaller(ctx context.Context, spec ComponentSpec) context.Context {
	return bus.WithCallerLabels(ctx, spec.Self(), spec.Labels)
}

func newPanicError(name string, value any) *PanicError {
	return &PanicError{
		Component: name,
		Value:     value,
		Stack:     debug.Stack(),
	}
}

func (r *Runtime) factoryFor(kind string) (ServiceFactory, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	factory, ok := r.factories[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoFactory, kind)
	}
	return factory, nil
}

func (r *Runtime) rootContext() (context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || r.root == nil {
		return nil, ErrRuntimeNotStarted
	}
	return r.root, nil
}

func (r *Runtime) readyTimeoutOf() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return opt.Positive(r.readyTimeout, ReadyTimeout)
}

func (r *Runtime) backoffFor(restarts int32) time.Duration {
	r.mu.Lock()
	initial := r.restartBackoff
	max := r.maxRestartBackoff
	r.mu.Unlock()
	initial = opt.Positive(initial, DefaultRestartBackoff)
	if max < initial {
		max = initial
	}
	delay := initial
	for i := int32(1); i < restarts; i++ {
		if delay >= max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		delay = max
	}
	return delay
}

func stopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, DefaultStopTimeout)
}

func (i *instance) name() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.key.Kind + "/" + i.key.Name
}

func (i *instance) restartsOf() int32 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.restarts
}

func (i *instance) startedAtOf() time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.startedAt
}

func (i *instance) isStopping() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.stopping
}

func (i *instance) isReady(attempt uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.attempt == attempt && i.running && i.svc != nil && !i.stopping && i.status.Phase == PhaseReady && i.status.Ready
}

func (i *instance) deactivate(attempt uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.attempt == attempt {
		i.running = false
	}
}

func (i *instance) claimStop() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stopClaimed {
		return false
	}
	i.stopClaimed = true
	return true
}

func (i *instance) specSnapshot() ComponentSpec {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.spec
}

func (i *instance) updateStatus(attempt uint64, status ComponentStatus, restarts int32, running bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.attempt != attempt {
		return
	}
	i.status = status
	i.restarts = restarts
	i.running = running
}

func statusFromSpec(spec ComponentSpec) ComponentStatus {
	return ComponentStatus{
		ObservedGeneration: spec.Generation,
		Phase:              PhasePending,
	}
}

func startingStatus(spec ComponentSpec, restarts int32) ComponentStatus {
	st := statusFromSpec(spec)
	st.Phase = PhaseStarting
	st.Restarts = restarts
	conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "Starting", "the service is starting")
	return st
}

func failedStatus(spec ComponentSpec, restarts int32, err error) ComponentStatus {
	st := statusFromSpec(spec)
	st.Phase = PhaseFailed
	st.Restarts = restarts
	if err != nil {
		st.Message = err.Error()
	}
	conds.Set(&st.Conditions, spec.Generation, ConditionReady, false, "Failed", st.Message)
	return st
}

func cloneStatus(status ComponentStatus) ComponentStatus {
	status.Endpoints = cloneEndpoints(status.Endpoints)
	status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	return status
}

func restartAllowed(policy RestartPolicy, err error) bool {
	switch policy.WithDefault() {
	case RestartNever:
		return false
	case RestartOnFailure:
		return err != nil
	default:
		return true
	}
}

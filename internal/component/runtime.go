package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
)

const (
	DefaultRestartBackoff    = time.Second
	DefaultMaxRestartBackoff = 30 * time.Second
	DefaultStopTimeout       = 30 * time.Second
	ReadyPollInterval        = 500 * time.Millisecond
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

type ServiceFactory func(ctx context.Context, deps Deps, spec ComponentSpec) (Service, error)

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
	deps Deps

	makeStateDir func(dir string) error

	mu        sync.Mutex
	instances map[types.UID]*instance
	factories map[string]ServiceFactory
	root      context.Context
	cancel    context.CancelFunc
	started   bool
	closed    bool

	callbackMu sync.Mutex
	onStatus   StatusCallback

	restartBackoff    time.Duration
	maxRestartBackoff time.Duration
	readyPoll         time.Duration
	readyTimeout      time.Duration
}

type instance struct {
	opMu sync.Mutex
	mu   sync.Mutex

	uid         types.UID
	key         Key
	spec        ComponentSpec
	svc         Service
	deps        Deps
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

func NewRuntime(deps Deps, onStatus StatusCallback) *Runtime {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.Registry == nil {
		deps.Registry = NewRegistry()
	}
	if deps.Handlers == nil {
		deps.Handlers = NewHandlerRegistry()
	}
	return &Runtime{
		deps:              deps,
		instances:         map[types.UID]*instance{},
		factories:         map[string]ServiceFactory{},
		onStatus:          onStatus,
		restartBackoff:    DefaultRestartBackoff,
		maxRestartBackoff: DefaultMaxRestartBackoff,
		readyPoll:         ReadyPollInterval,
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
	return r.deps.Registry
}

func (r *Runtime) Handlers() *HandlerRegistry {
	return r.deps.Handlers
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
	if r.deps.Handlers != nil {
		if err := r.deps.Handlers.Stop(stopCtx); err != nil {
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
		setReadyCondition(&st, spec.Generation, false, "InvalidSpec", err.Error())
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
	changed := inst.spec.Generation != spec.Generation || inst.spec.RestartStamp() != spec.RestartStamp()
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
		setReadyCondition(&st, spec.Generation, false, "Deleted", st.Message)
		r.emit(ctx, spec, st)
		return st, stopErr
	}
	if spec.Suspend {
		stopErr := r.stopInstanceLocked(ctx, inst)
		st := statusFromSpec(spec)
		st.Phase = PhaseSuspended
		st.Message = "the component is suspended"
		st.Restarts = restarts
		setReadyCondition(&st, spec.Generation, false, "Suspended", st.Message)
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
}

func (r *Runtime) startLocked(ctx context.Context, inst *instance, spec ComponentSpec) (ComponentStatus, error) {
	factory, err := r.factoryFor(spec.Kind)
	if err != nil {
		st := failedStatus(spec, inst.restartsOf(), err)
		r.emit(ctx, spec, st)
		return st, err
	}
	root, err := r.rootContext()
	if err != nil {
		st := failedStatus(spec, inst.restartsOf(), err)
		r.emit(ctx, spec, st)
		return st, err
	}
	deps := r.deps
	deps.StateDir = spec.ComponentStateDir(deps.StateDir)
	if deps.StateDir == "" {
		err := fmt.Errorf("component: %s/%s has no state dir", spec.Kind, spec.Name)
		st := failedStatus(spec, inst.restartsOf(), err)
		r.emit(ctx, spec, st)
		return st, err
	}
	if err := r.prepareStateDir(deps.StateDir); err != nil {
		err := fmt.Errorf("component: creating the state dir for %s/%s: %w", spec.Kind, spec.Name, err)
		st := failedStatus(spec, inst.restartsOf(), err)
		r.emit(ctx, spec, st)
		return st, err
	}
	svc, err := r.callFactory(ctx, spec, factory, deps)
	if err != nil {
		err := fmt.Errorf("component: building %s/%s: %w", spec.Kind, spec.Name, err)
		st := failedStatus(spec, inst.restartsOf(), err)
		r.emit(ctx, spec, st)
		return st, err
	}
	if svc == nil {
		err := fmt.Errorf("component: the factory for %s/%s returned no service", spec.Kind, spec.Name)
		st := failedStatus(spec, inst.restartsOf(), err)
		r.emit(ctx, spec, st)
		return st, err
	}
	runCtx, cancel := context.WithCancel(specCaller(root, spec))
	done := make(chan struct{})
	startedAt := time.Now().UTC()
	inst.mu.Lock()
	restarts := inst.restarts
	inst.key = Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name}
	inst.spec = spec
	inst.svc = svc
	inst.deps = deps
	inst.cancel = cancel
	inst.done = done
	inst.startedAt = startedAt
	inst.attempt++
	attempt := inst.attempt
	inst.status = startingStatus(spec, restarts)
	inst.status.StartedAt = startedAt
	st := inst.status
	inst.running = true
	inst.started = true
	inst.ended = false
	inst.stopping = false
	inst.stopClaimed = false
	inst.supervising = true
	inst.mu.Unlock()

	r.emit(ctx, spec, st)
	go r.supervise(inst, spec, deps, svc, runCtx, done, attempt)
	return st, nil
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
	inst.mu.Unlock()

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

func (r *Runtime) supervise(inst *instance, spec ComponentSpec, deps Deps, svc Service, runCtx context.Context, done chan struct{}, attempt uint64) {
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
	restarts := inst.restartsOf()
	for {
		if svc == nil {
			next, ferr := r.buildService(runCtx, spec, deps)
			if ferr != nil {
				restarts++
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
		ready := r.launchReady(inst, svc, runCtx, attempt, started)
		close(started)
		err := r.callStart(runCtx, svc, deps, spec)
		if runCtx.Err() != nil || inst.isStopping() {
			inst.deactivate(attempt)
			r.unregister(inst)
			return
		}
		if err == nil {
			if r.awaitDetached(runCtx, inst, attempt, ready) {
				inst.deactivate(attempt)
				r.unregister(inst)
				return
			}
		}
		inst.deactivate(attempt)
		r.unregister(inst)
		if inst.claimStop() {
			stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(runCtx), DefaultStopTimeout)
			if stopErr := r.callStop(stopCtx, spec.Kind+"/"+spec.Name, Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name}, svc); stopErr != nil {
				r.deps.Log.Warn("component: stopping the exited service failed", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace, "err", stopErr)
			}
			stopCancel()
		}
		if runCtx.Err() != nil || inst.isStopping() {
			return
		}
		restarts++
		if err != nil {
			r.deps.Log.Warn("component: service exited", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace, "restarts", restarts, "err", err)
			var panicErr *PanicError
			if errors.As(err, &panicErr) && len(panicErr.Stack) > 0 {
				r.deps.Log.Error("component: service panicked", "component", spec.Kind+"/"+spec.Name, "stack", string(panicErr.Stack))
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
		setReadyCondition(&st, spec.Generation, false, "ServiceExited", st.Message)
		inst.updateStatus(attempt, st, restarts, false)
		r.emit(context.WithoutCancel(runCtx), spec, st)
		if !restartAllowed(spec.RestartPolicy, err) || !r.waitRestart(runCtx, restarts) {
			return
		}
		svc = nil
	}
}

func (r *Runtime) buildService(ctx context.Context, spec ComponentSpec, deps Deps) (Service, error) {
	factory, err := r.factoryFor(spec.Kind)
	if err != nil {
		return nil, err
	}
	svc, err := r.callFactory(ctx, spec, factory, deps)
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

func (r *Runtime) launchReady(inst *instance, svc Service, ctx context.Context, attempt uint64, started <-chan struct{}) chan struct{} {
	ready := make(chan struct{})
	go r.awaitReady(inst, svc, ctx, attempt, started, ready)
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
	poll, timeout := r.readySettings()
	deadline := time.Now().Add(timeout)
	timedOut := false
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		inst.mu.Lock()
		current := inst.attempt == attempt && inst.running && inst.svc == svc && !inst.stopping
		inst.mu.Unlock()
		if !current {
			return
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, poll*2)
		err := r.callReady(probeCtx, inst, svc)
		probeCancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			var panicErr *PanicError
			if errors.As(err, &panicErr) {
				r.deps.Log.Error("component: service ready check panicked", "component", panicErr.Component, "stack", string(panicErr.Stack))
				return
			}
			if !timedOut && time.Now().After(deadline) {
				timedOut = true
				spec := inst.specSnapshot()
				restarts := inst.restartsOf()
				st := startingStatus(spec, restarts)
				st.StartedAt = inst.startedAtOf()
				st.Message = fmt.Sprintf("the service is not ready after %s: %v", timeout, err)
				setReadyCondition(&st, spec.Generation, false, "NotReady", st.Message)
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
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}
		endpoints, err := r.callEndpoints(inst, svc)
		if err != nil {
			r.deps.Log.Error("component: reading service endpoints failed", "err", err)
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
		setReadyCondition(&st, spec.Generation, true, "Ready", "the service reports ready")
		inst.status = st
		inst.mu.Unlock()
		if !r.register(inst, svc, endpoints) {
			return
		}
		r.emit(context.WithoutCancel(ctx), spec, st)
		close(ready)
		return
	}
}

func (r *Runtime) register(inst *instance, svc Service, endpoints []Endpoint) bool {
	if r.deps.Registry == nil {
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
	r.deps.Registry.Put(entry)
	return true
}

func (r *Runtime) unregister(inst *instance) {
	if r.deps.Registry == nil {
		return
	}
	inst.mu.Lock()
	key := inst.key
	uid := inst.uid
	inst.mu.Unlock()
	r.deps.Registry.Unregister(key, uid)
	if r.deps.Handlers != nil {
		r.deps.Handlers.Remove(key)
	}
}

func (r *Runtime) emit(ctx context.Context, spec ComponentSpec, status ComponentStatus) {
	defer func() {
		if value := recover(); value != nil {
			r.deps.Log.Error("component: status callback panicked",
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
		r.deps.Log.Warn("component: status callback failed", "component", spec.Kind+"/"+spec.Name, "workspace", spec.Workspace, "err", err)
	}
}

func (r *Runtime) callStart(ctx context.Context, svc Service, deps Deps, spec ComponentSpec) (err error) {
	name := spec.Kind + "/" + spec.Name
	defer func() {
		if value := recover(); value != nil {
			err = newPanicError(name, value)
		}
	}()
	return svc.Start(specCaller(ctx, spec), deps)
}

func (r *Runtime) callReady(ctx context.Context, inst *instance, svc Service) (err error) {
	name := inst.name()
	defer func() {
		if value := recover(); value != nil {
			err = newPanicError(name, value)
		}
	}()
	return svc.Ready(specCaller(ctx, inst.specSnapshot()))
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

func (r *Runtime) callFactory(ctx context.Context, spec ComponentSpec, factory ServiceFactory, deps Deps) (svc Service, err error) {
	name := spec.Kind + "/" + spec.Name
	defer func() {
		if value := recover(); value != nil {
			svc = nil
			err = newPanicError(name, value)
		}
	}()
	return factory(specCaller(ctx, spec), deps, spec)
}

func specCaller(ctx context.Context, spec ComponentSpec) context.Context {
	return bus.WithCallerLabels(ctx, commonv1.ObjectRef{Group: spec.Group, Kind: spec.Kind, Name: spec.Name, Workspace: spec.Workspace}, spec.Labels)
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

func (r *Runtime) readySettings() (time.Duration, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poll := r.readyPoll
	if poll <= 0 {
		poll = ReadyPollInterval
	}
	timeout := r.readyTimeout
	if timeout <= 0 {
		timeout = ReadyTimeout
	}
	return poll, timeout
}

func (r *Runtime) backoffFor(restarts int32) time.Duration {
	r.mu.Lock()
	initial := r.restartBackoff
	max := r.maxRestartBackoff
	r.mu.Unlock()
	if initial <= 0 {
		initial = DefaultRestartBackoff
	}
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

func (i *instance) keyOf() Key {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.key
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
	setReadyCondition(&st, spec.Generation, false, "Starting", "the service is starting")
	return st
}

func failedStatus(spec ComponentSpec, restarts int32, err error) ComponentStatus {
	st := statusFromSpec(spec)
	st.Phase = PhaseFailed
	st.Restarts = restarts
	if err != nil {
		st.Message = err.Error()
	}
	setReadyCondition(&st, spec.Generation, false, "Failed", st.Message)
	return st
}

func setReadyCondition(st *ComponentStatus, generation int64, ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
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

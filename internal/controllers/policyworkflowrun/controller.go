package policyworkflowrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	pecontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
)

const (
	Name = "policyworkflowrun"

	Export = "policy"

	Finalizer = "policy.socialweb.computer/run"

	writeAttempts = 5

	writeBackoff = 500 * time.Millisecond
)

type Options struct {
	Engines *pecontroller.EngineManager

	Runs *pecontroller.RunReconciler

	DefaultRunTTL time.Duration

	Log *slog.Logger

	Now func() time.Time
}

type activeRun struct {
	uid types.UID

	cancel context.CancelFunc
}

type Controller struct {
	engines *pecontroller.EngineManager

	runs pecontroller.RunReconciler

	ttl time.Duration

	log *slog.Logger

	now func() time.Time

	mu sync.Mutex

	active map[string]*activeRun

	terminal map[string]policyv1alpha1.PolicyWorkflowRunStatus

	waiting map[string]map[string]struct{}

	wg sync.WaitGroup
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Engines == nil {
		return nil, errors.New("policyworkflowrun: the engine manager is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &Controller{engines: opts.Engines, ttl: opts.DefaultRunTTL, log: log.With("component", "policyworkflowrun-controller"), now: now,
		active: map[string]*activeRun{}, terminal: map[string]policyv1alpha1.PolicyWorkflowRunStatus{}, waiting: map[string]map[string]struct{}{}}
	if opts.Runs != nil {
		c.runs = *opts.Runs
	}
	if err := ctlruntime.For[*policyv1alpha1.PolicyWorkflowRun](m, Name, c, ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer),
		ctlruntime.Watches[*policyv1alpha1.PolicyEngine](c.engineChanged),
		ctlruntime.Watches[*policyv1alpha1.PolicyWorkflow](c.workflowChanged)); err != nil {
		return nil, fmt.Errorf("policyworkflowrun: registering the PolicyWorkflowRun controller: %w", err)
	}
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, run *policyv1alpha1.PolicyWorkflowRun) (ctlruntime.Result, error) {
	key := pecontroller.EngineKey(req.Cluster, run.Name)
	if !run.DeletionTimestamp.IsZero() {
		c.cancelActive(key)
		c.forgetTerminal(key)
		return ctlruntime.Result{}, nil
	}
	if policy.Terminal(run.Status.Phase) {
		c.cancelActive(key)
		c.forgetTerminal(key)
		return c.expire(ctx, req, run)
	}
	if run.Spec.Cancel || run.Spec.Suspend {
		c.forgetTerminal(key)
		var err error
		if !c.isActive(key) {
			status := pecontroller.CancelledStatus(run)
			if !run.Spec.Cancel {
				status = pecontroller.SuspendedStatus(run)
			}
			err = c.write(ctx, req, run, status)
		}
		c.cancelActive(key)
		return ctlruntime.Result{}, err
	}
	if status, ok := c.terminalStatus(key); ok {
		c.cancelActive(key)
		if err := c.write(ctx, req, run, status); err != nil {
			return ctlruntime.Result{}, err
		}
		c.forgetTerminal(key)
		return ctlruntime.Result{}, nil
	}
	if c.activeFor(key, run.UID) {
		return ctlruntime.Result{}, nil
	}
	deadline, hasDeadline := pecontroller.RunDeadline(run)
	if hasDeadline && !c.now().Before(deadline) {
		return ctlruntime.Result{}, c.write(ctx, req, run, pecontroller.DeadlineStatus(run))
	}
	engine, ok := c.engineFor(ctx, req, run.Spec.EngineRef)
	if !ok {
		c.wait(req.Cluster, run.Spec.EngineRef, run.Name)
		message := "the run is waiting for a ready policy engine"
		if run.Spec.EngineRef != "" {
			message = fmt.Sprintf("the run is waiting for policy engine %s", run.Spec.EngineRef)
		}
		if err := c.write(ctx, req, run, pecontroller.PendingStatus(run, "EngineUnavailable", message)); err != nil {
			return ctlruntime.Result{}, err
		}
		if hasDeadline {
			return ctlruntime.Result{RequeueAfter: deadline.Sub(c.now())}, nil
		}
		return ctlruntime.Result{}, nil
	}
	return ctlruntime.Result{}, c.start(ctx, req, run, engine)
}

func (c *Controller) engineFor(ctx context.Context, req ctlruntime.Request, name string) (*pecontroller.Engine, bool) {
	if name == "" {
		engine, ok := c.engines.Only(req.Cluster + "/")
		return engine, ok && engine.Ready()
	}
	key := pecontroller.EngineKey(req.Cluster, name)
	if engine, ok := c.engines.EngineFor(key); ok {
		return engine, engine.Ready()
	}
	object := &policyv1alpha1.PolicyEngine{}
	if err := req.Get(ctx, "", name, object); err != nil || !object.DeletionTimestamp.IsZero() || object.Spec.Suspend {
		return nil, false
	}
	if _, err := c.engines.ReconcilePolicyEngine(ctx, key, object); err != nil {
		return nil, false
	}
	engine, ok := c.engines.EngineFor(key)
	return engine, ok && engine.Ready()
}

func (c *Controller) start(ctx context.Context, req ctlruntime.Request, run *policyv1alpha1.PolicyWorkflowRun, engine *pecontroller.Engine) error {
	key := pecontroller.EngineKey(req.Cluster, run.Name)
	starting := pecontroller.StartingStatus(run, c.now())
	runCtx, cancel := context.WithCancel(ctx)
	handle := &activeRun{uid: run.UID, cancel: cancel}
	c.mu.Lock()
	c.active[key] = handle
	c.mu.Unlock()
	if err := c.write(ctx, req, run, starting); err != nil {
		c.forgetActive(key, handle)
		cancel()
		return err
	}
	runCopy := run.DeepCopy()
	runCopy.Status = starting
	reconciler := c.runs
	reconciler.Engine = engine
	reconciler.Workflows = workflowSource(req)
	c.wg.Go(func() {
		err := supervise.Run(runCtx, "policy run "+key, func(runCtx context.Context) error {
			outcome, err := reconciler.ReconcilePolicyWorkflowRun(runCtx, runCopy)
			if err != nil {
				c.log.Warn("executing a policy run", "run", key, "err", err.Error())
			}
			c.finalize(runCtx, req, key, handle, runCopy, outcome)
			return nil
		})
		if err != nil {
			c.log.Error("the policy run stopped", "run", key, "err", err.Error())
		}
	})
	return nil
}

func (c *Controller) finalize(runCtx context.Context, req ctlruntime.Request, key string, handle *activeRun, run *policyv1alpha1.PolicyWorkflowRun, outcome pecontroller.Outcome) {
	defer c.forgetActive(key, handle)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), pecontroller.DefaultStatusTimeout)
	defer cancel()
	var err error
	for attempt := range writeAttempts {
		current := &policyv1alpha1.PolicyWorkflowRun{}
		if err = req.Get(ctx, "", run.Name, current); err != nil {
			if apierrors.IsNotFound(err) {
				return
			}
		} else if !current.DeletionTimestamp.IsZero() || current.UID != run.UID {
			return
		} else {
			status := pecontroller.FinalStatus(current, outcome, c.now())
			if err = c.write(ctx, req, current, status); err == nil {
				c.forgetTerminal(key)
				return
			}
			c.rememberTerminal(key, status)
		}
		timer := time.NewTimer(writeBackoff << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			c.log.Warn("writing a finished policy run", "run", key, "err", err.Error())
			return
		case <-timer.C:
		}
	}
	c.log.Warn("writing a finished policy run", "run", key, "err", err.Error())
}

func (c *Controller) expire(ctx context.Context, req ctlruntime.Request, run *policyv1alpha1.PolicyWorkflowRun) (ctlruntime.Result, error) {
	var workflow *policyv1alpha1.PolicyWorkflow
	if run.Spec.TTLSecondsAfterFinished == nil && run.Spec.WorkflowRef != "" {
		c.wait(req.Cluster, "workflow/"+run.Spec.WorkflowRef, run.Name)
		object := &policyv1alpha1.PolicyWorkflow{}
		if err := req.Get(ctx, "", run.Spec.WorkflowRef, object); err == nil {
			workflow = object
		}
	}
	ttl, ok := pecontroller.RunTTL(run, workflow, c.ttl)
	if !ok {
		return ctlruntime.Result{}, nil
	}
	if remaining := pecontroller.RunExpiry(run, ttl).Sub(c.now()); remaining > 0 {
		return ctlruntime.Result{RequeueAfter: remaining}, nil
	}
	return ctlruntime.Result{}, req.Delete(ctx, run)
}

func (c *Controller) write(ctx context.Context, req ctlruntime.Request, run *policyv1alpha1.PolicyWorkflowRun, status policyv1alpha1.PolicyWorkflowRunStatus) error {
	if !run.DeletionTimestamp.IsZero() || pecontroller.RunStatusEqual(run.Status, status) {
		return nil
	}
	object := run.DeepCopy()
	object.Status = status
	if err := req.ApplyStatus(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Controller) engineChanged(_ context.Context, src ctlruntime.Request, engine *policyv1alpha1.PolicyEngine) []ctlruntime.Request {
	return append(c.take(src.Cluster, engine.Name), c.take(src.Cluster, "")...)
}

func (c *Controller) workflowChanged(_ context.Context, src ctlruntime.Request, workflow *policyv1alpha1.PolicyWorkflow) []ctlruntime.Request {
	return c.take(src.Cluster, "workflow/"+workflow.Name)
}

func (c *Controller) wait(cluster, dependency, run string) {
	key := pecontroller.EngineKey(cluster, dependency)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waiting[key] == nil {
		c.waiting[key] = map[string]struct{}{}
	}
	c.waiting[key][run] = struct{}{}
}

func (c *Controller) take(cluster, dependency string) []ctlruntime.Request {
	key := pecontroller.EngineKey(cluster, dependency)
	c.mu.Lock()
	runs := c.waiting[key]
	delete(c.waiting, key)
	c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(runs))
	for name := range runs {
		out = append(out, ctlruntime.Request{Cluster: cluster, Name: name})
	}
	return out
}

func (c *Controller) isActive(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.active[key]
	return ok
}

func (c *Controller) activeFor(key string, uid types.UID) bool {
	c.mu.Lock()
	handle := c.active[key]
	if handle == nil || handle.uid == uid {
		c.mu.Unlock()
		return handle != nil
	}
	delete(c.active, key)
	c.mu.Unlock()
	handle.cancel()
	return false
}

func (c *Controller) cancelActive(key string) {
	c.mu.Lock()
	handle := c.active[key]
	delete(c.active, key)
	c.mu.Unlock()
	if handle != nil {
		handle.cancel()
	}
}

func (c *Controller) forgetActive(key string, handle *activeRun) {
	c.mu.Lock()
	if c.active[key] == handle {
		delete(c.active, key)
	}
	c.mu.Unlock()
}

func (c *Controller) rememberTerminal(key string, status policyv1alpha1.PolicyWorkflowRunStatus) {
	c.mu.Lock()
	c.terminal[key] = status
	c.mu.Unlock()
}

func (c *Controller) terminalStatus(key string) (policyv1alpha1.PolicyWorkflowRunStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.terminal[key]
	return status, ok
}

func (c *Controller) forgetTerminal(key string) {
	c.mu.Lock()
	delete(c.terminal, key)
	c.mu.Unlock()
}

func workflowSource(req ctlruntime.Request) pecontroller.WorkflowSource {
	return pecontroller.WorkflowSourceFunc(func(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflow, error) {
		workflow := &policyv1alpha1.PolicyWorkflow{}
		if err := req.Get(ctx, "", name, workflow); err != nil {
			return nil, err
		}
		return workflow, nil
	})
}

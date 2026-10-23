package policytrigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	pecontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
)

const (
	Name = "policytrigger"

	Export = "policy"
)

type ActionsFunc func(workspace string) (map[string]pecontroller.ActionHandler, error)

type Options struct {
	Actions ActionsFunc

	Log *slog.Logger
}

type Controller struct {
	actions ActionsFunc

	log *slog.Logger

	mu sync.Mutex

	handlers map[string]map[string]pecontroller.ActionHandler

	triggers map[string]map[string]struct{}

	runs map[string]map[string]*policyv1alpha1.PolicyWorkflowRun
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{actions: opts.Actions, log: log.With("component", "policytrigger-controller"),
		handlers: map[string]map[string]pecontroller.ActionHandler{}, triggers: map[string]map[string]struct{}{},
		runs: map[string]map[string]*policyv1alpha1.PolicyWorkflowRun{}}
	if err := ctlruntime.For[*policyv1alpha1.PolicyTrigger](m, Name, c, ctlruntime.Export(Export),
		ctlruntime.Watches[*policyv1alpha1.PolicyWorkflowRun](c.runChanged)); err != nil {
		return nil, fmt.Errorf("policytrigger: registering the PolicyTrigger controller: %w", err)
	}
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, trigger *policyv1alpha1.PolicyTrigger) (ctlruntime.Result, error) {
	c.track(req.Cluster, trigger.Name, trigger.DeletionTimestamp.IsZero())
	handlers, err := c.handlersFor(req)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	reconciler := &pecontroller.TriggerReconciler{Handlers: handlers}
	state, err := reconciler.ReconcilePolicyTrigger(ctx, trigger)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	state.LastRun = trigger.Status.LastRun
	state.NextRun = trigger.Status.NextRun
	if !pecontroller.TriggerStatusEqual(trigger.Status, state) {
		if err := c.apply(ctx, req, trigger, state); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	runs := c.finished(req.Cluster)
	if len(runs) == 0 {
		return ctlruntime.Result{}, nil
	}
	dispatchErr := reconciler.DispatchRuns(ctx, trigger, runs, func(state pecontroller.TriggerReconcileState) error {
		return c.apply(ctx, req, trigger, state)
	})
	if dispatchErr != nil {
		c.log.Warn("dispatching a policy trigger", "trigger", trigger.Name, "workspace", req.Workspace, "err", dispatchErr.Error())
	}
	return ctlruntime.Result{}, dispatchErr
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, trigger *policyv1alpha1.PolicyTrigger, state pecontroller.TriggerReconcileState) error {
	object := trigger.DeepCopy()
	object.Status = policyv1alpha1.PolicyTriggerStatus{LastRun: state.LastRun, NextRun: state.NextRun, Active: state.Active, Conditions: state.Conditions}
	if err := req.ApplyStatus(ctx, object); err != nil {
		return err
	}
	trigger.Status = object.Status
	return nil
}

func (c *Controller) handlersFor(req ctlruntime.Request) (map[string]pecontroller.ActionHandler, error) {
	c.mu.Lock()
	handlers, ok := c.handlers[req.Cluster]
	c.mu.Unlock()
	if ok {
		return handlers, nil
	}
	if c.actions == nil {
		return nil, nil
	}
	handlers, err := c.actions(req.Workspace)
	if err != nil {
		return nil, errors.Join(errors.New("policytrigger: the trigger action handlers"), err)
	}
	c.mu.Lock()
	c.handlers[req.Cluster] = handlers
	c.mu.Unlock()
	return handlers, nil
}

func (c *Controller) track(cluster, name string, live bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !live {
		delete(c.triggers[cluster], name)
		return
	}
	if c.triggers[cluster] == nil {
		c.triggers[cluster] = map[string]struct{}{}
	}
	c.triggers[cluster][name] = struct{}{}
}

func (c *Controller) finished(cluster string) []*policyv1alpha1.PolicyWorkflowRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*policyv1alpha1.PolicyWorkflowRun, 0, len(c.runs[cluster]))
	for _, run := range c.runs[cluster] {
		out = append(out, run)
	}
	return out
}

func (c *Controller) runChanged(_ context.Context, src ctlruntime.Request, run *policyv1alpha1.PolicyWorkflowRun) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !run.DeletionTimestamp.IsZero() || !pecontroller.Finished(run) {
		delete(c.runs[src.Cluster], run.Name)
		return nil
	}
	if c.runs[src.Cluster] == nil {
		c.runs[src.Cluster] = map[string]*policyv1alpha1.PolicyWorkflowRun{}
	}
	c.runs[src.Cluster][run.Name] = run.DeepCopy()
	out := make([]ctlruntime.Request, 0, len(c.triggers[src.Cluster]))
	for name := range c.triggers[src.Cluster] {
		out = append(out, ctlruntime.Request{Cluster: src.Cluster, Name: name})
	}
	return out
}

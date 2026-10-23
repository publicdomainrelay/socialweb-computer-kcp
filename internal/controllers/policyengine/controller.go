package policyengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/runtime/schema"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	pecontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
)

const (
	Name = "policyengine"

	Export = "policy"

	Finalizer = "policy.socialweb.computer/engine"
)

var GroupKind = schema.GroupKind{Group: policyv1alpha1.GroupName, Kind: "PolicyEngine"}

type Options struct {
	Bus *bus.Bus

	Engines *pecontroller.EngineManager

	Log *slog.Logger
}

type Controller struct {
	engines *pecontroller.EngineManager

	runner *Runner

	log *slog.Logger
}

func Key(cluster, name string) string {
	return pecontroller.EngineKey(cluster, name)
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Engines == nil {
		return nil, errors.New("policyengine: the engine manager is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "policyengine-controller")
	c := &Controller{engines: opts.Engines, runner: newRunner(log), log: log}
	if err := ctlruntime.For[*policyv1alpha1.PolicyEngine](m, Name, c, ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer),
		ctlruntime.Watches[*policyv1alpha1.PolicyWorkflowRun](c.observeRun)); err != nil {
		return nil, fmt.Errorf("policyengine: registering the PolicyEngine controller: %w", err)
	}
	if opts.Bus != nil {
		bus.Provide[policy.Runner](opts.Bus, GroupKind, policy.CapabilityRun, c.runner.open)
	}
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, engine *policyv1alpha1.PolicyEngine) (ctlruntime.Result, error) {
	key := Key(req.Cluster, engine.Name)
	state, err := c.engines.ReconcilePolicyEngine(ctx, key, engine)
	if _, running := c.engines.EngineFor(key); running && engine.DeletionTimestamp.IsZero() {
		c.runner.bind(req, engine.Name)
	} else {
		c.runner.unbind(req, engine.Name)
	}
	if !engine.DeletionTimestamp.IsZero() || pecontroller.EngineStatusEqual(engine.Status, state) {
		return ctlruntime.Result{}, err
	}
	engine.Status = policyv1alpha1.PolicyEngineStatus{Phase: state.Phase, Ready: state.Ready, ActiveRuns: state.ActiveRuns, Conditions: state.Conditions}
	if applyErr := req.ApplyStatus(ctx, engine); applyErr != nil {
		return ctlruntime.Result{}, errors.Join(err, applyErr)
	}
	return ctlruntime.Result{}, err
}

func (c *Controller) observeRun(_ context.Context, src ctlruntime.Request, run *policyv1alpha1.PolicyWorkflowRun) []ctlruntime.Request {
	c.runner.observe(src.Cluster, run)
	if run.Spec.EngineRef == "" {
		return nil
	}
	return []ctlruntime.Request{{Cluster: src.Cluster, Name: run.Spec.EngineRef}}
}

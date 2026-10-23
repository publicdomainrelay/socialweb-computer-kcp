package policyworkflow

import (
	"context"
	"errors"
	"fmt"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	pecontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
)

const (
	Name = "policyworkflow"

	Export = "policy"
)

type Options struct {
	Workflows *pecontroller.WorkflowReconciler
}

type Controller struct {
	workflows *pecontroller.WorkflowReconciler
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	workflows := opts.Workflows
	if workflows == nil {
		workflows = &pecontroller.WorkflowReconciler{}
	}
	c := &Controller{workflows: workflows}
	if err := ctlruntime.For[*policyv1alpha1.PolicyWorkflow](m, Name, c, ctlruntime.Export(Export)); err != nil {
		return nil, fmt.Errorf("policyworkflow: registering the PolicyWorkflow controller: %w", err)
	}
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, workflow *policyv1alpha1.PolicyWorkflow) (ctlruntime.Result, error) {
	state, err := c.workflows.ReconcilePolicyWorkflow(ctx, workflow)
	if ctx.Err() != nil {
		return ctlruntime.Result{}, err
	}
	if pecontroller.WorkflowStatusEqual(workflow.Status, state, workflow.Generation) {
		return ctlruntime.Result{}, err
	}
	workflow.Status = policyv1alpha1.PolicyWorkflowStatus{Phase: state.Phase, ObservedGeneration: workflow.Generation, Conditions: state.Conditions}
	if applyErr := req.ApplyStatus(ctx, workflow); applyErr != nil {
		return ctlruntime.Result{}, errors.Join(err, applyErr)
	}
	return ctlruntime.Result{}, err
}

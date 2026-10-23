package controller

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type EngineReconcileState struct {
	Phase      policyv1alpha1.PolicyEnginePhase
	Ready      bool
	ActiveRuns int32
	Conditions []metav1.Condition
}

type WorkflowReconcileState struct {
	Phase      policyv1alpha1.PolicyWorkflowPhase
	Conditions []metav1.Condition
}

type TriggerReconcileState struct {
	LastRun    *metav1.Time
	NextRun    *metav1.Time
	Active     int32
	Conditions []metav1.Condition
}

type PolicyEngineController interface {
	ReconcilePolicyEngine(ctx context.Context, engine *policyv1alpha1.PolicyEngine) (EngineReconcileState, error)
}

type PolicyWorkflowController interface {
	ReconcilePolicyWorkflow(ctx context.Context, workflow *policyv1alpha1.PolicyWorkflow) (WorkflowReconcileState, error)
}

type PolicyWorkflowRunController interface {
	ReconcilePolicyWorkflowRun(ctx context.Context, run *policyv1alpha1.PolicyWorkflowRun) (Outcome, error)
}

type PolicyTriggerController interface {
	ReconcilePolicyTrigger(ctx context.Context, trigger *policyv1alpha1.PolicyTrigger) (TriggerReconcileState, error)
}

type WorkflowSource interface {
	GetPolicyWorkflow(ctx context.Context, name string) (*policyv1alpha1.PolicyWorkflow, error)
}

type RecordSource interface {
	ResolveRecord(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error)
	ResolveWorkflow(ctx context.Context, ref commonv1.StrongRef) (json.RawMessage, error)
}

type ActionHandler interface {
	Kind() string
	Dispatch(ctx context.Context, spec json.RawMessage, outcome Outcome) error
}

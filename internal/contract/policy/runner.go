package policy

import (
	"context"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
)

const CapabilityRun = "policy.run"

type Owner struct {
	APIVersion string

	Kind string

	Name string

	UID string
}

type RunRequest struct {
	Name string

	Workflow string

	Owner Owner

	Spec policyv1alpha1.PolicyWorkflowRunSpec
}

type Runner interface {
	Run(ctx context.Context, req RunRequest) (string, error)

	Status(ctx context.Context, runID string) (policyv1alpha1.PolicyWorkflowRunStatus, error)

	Wait(ctx context.Context, runID string) (policyv1alpha1.PolicyWorkflowRunStatus, error)
}

func Terminal(phase policyv1alpha1.PolicyWorkflowRunPhase) bool {
	switch phase {
	case policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed, policyv1alpha1.PolicyWorkflowRunCancelled:
		return true
	default:
		return false
	}
}

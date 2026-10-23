package policy

import "context"

const CapabilityRun = "policy.run"

type RunRequest struct {
	Workflow string

	Inputs map[string]string
}

type Runner interface {
	Run(ctx context.Context, req RunRequest) (string, error)

	Status(ctx context.Context, runID string) (Task, error)
}

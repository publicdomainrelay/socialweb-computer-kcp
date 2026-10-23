package policy

import "context"

const (
	StateRunning = "running"

	StateSucceeded = "succeeded"

	StateFailed = "failed"
)

type Task struct {
	State string

	ExitStatus string

	Outputs map[string]string

	Message string
}

type Engine interface {
	Submit(ctx context.Context, workflow []byte, inputs map[string]string) (string, error)

	Status(ctx context.Context, taskID string) (Task, error)
}

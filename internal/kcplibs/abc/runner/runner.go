package runner

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/listeners"
)

type State string

const (
	StateRunning State = "running"

	StateSucceeded State = "succeeded"

	StateFailed State = "failed"
)

const (
	ReasonOOMKilled = "OOMKilled"

	ReasonHeapLimitExceeded = "HeapLimitExceeded"
)

type PodRequest struct {
	Name string

	Service string

	LogicalCluster string

	DenoJSON string

	DenoLock string

	Script string

	PermissionArgs []string

	Env map[string]string

	Token string

	Server string

	Workspace string

	MemoryMax uint64
}

type PodStatus struct {
	State State

	ExitCode int32

	Message string

	Reason string

	Outputs map[string]string

	BoundPort int32

	Listeners []listeners.Listener
}

type PodRunner interface {
	Start(ctx context.Context, req PodRequest) (string, error)

	Observe(ctx context.Context, runID string) (PodStatus, error)

	Stop(ctx context.Context, runID string) error

	Probe(ctx context.Context, runID string, command []string, timeout time.Duration) (bool, error)
}

type EngineRequest struct {
	Name string

	LogicalCluster string

	Port int

	Env map[string]string
}

type EngineStatus struct {
	State State

	Message string
}

type EngineRunner interface {
	Start(ctx context.Context, req EngineRequest) (string, error)

	Observe(ctx context.Context, runID string) (EngineStatus, error)

	Stop(ctx context.Context, runID string) error

	Probe(ctx context.Context, runID string, command []string, timeout time.Duration) (bool, error)
}

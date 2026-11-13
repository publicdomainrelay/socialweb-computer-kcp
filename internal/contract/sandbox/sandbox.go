package sandbox

import (
	"context"
	"time"
)

const OutputLimit = 64 << 10

type Program struct {
	Source string

	Dir string

	Timeout time.Duration

	Net bool

	PublicNetOnly bool

	Env map[string]string
}

type Result struct {
	ExitCode int

	TimedOut bool

	Stdout string

	Stderr string

	StdoutTruncated bool
}

type Runner interface {
	Run(ctx context.Context, p Program) (Result, error)

	Available() bool

	Fenced() bool

	Kind() string
}

type Fence struct {
	Fenced bool

	Reason string

	Message string
}

type Prober interface {
	Probe(ctx context.Context) (Fence, error)
}

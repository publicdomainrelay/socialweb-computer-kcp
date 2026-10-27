//go:build !linux && !darwin

package hostsvc

import (
	"context"
	"fmt"
	"runtime"
)

func Open(ctx context.Context, cfg Config) (Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: the %s backend is unsupported on %s; see plan 0011/0012", ErrUnsupported, cfg.Backend, runtime.GOOS)
}

func ListenPort(pid int) (int, error) {
	return 0, fmt.Errorf("%w: reading a bound port from /proc on %s; see plan 0011/0012", ErrUnsupported, runtime.GOOS)
}

func ListenPorts(pid int) ([]int, error) {
	return nil, fmt.Errorf("%w: reading bound ports from /proc on %s; see plan 0011/0012", ErrUnsupported, runtime.GOOS)
}

func OpenProcClient(ctx context.Context, cfg Config) (ProcClient, error) {
	return nil, fmt.Errorf("%w: the proc backend is unsupported on %s; see plan 0011/0012", ErrUnsupported, runtime.GOOS)
}

func (Processes) Wait(ctx context.Context, pid int, startTicks uint64) error {
	return fmt.Errorf("%w: waiting for pid %d on %s; see plan 0011/0012", ErrUnsupported, pid, runtime.GOOS)
}

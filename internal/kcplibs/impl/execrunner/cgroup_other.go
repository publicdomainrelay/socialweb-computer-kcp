//go:build !linux

package execrunner

import (
	"fmt"
	"os/exec"
	"runtime"
)

type limiter struct{}

func newLimiter(*exec.Cmd, string, uint64, Holder) (*limiter, error) {
	return nil, fmt.Errorf("%w: a pod memory limit on %s", ErrUnsupported, runtime.GOOS)
}

func (*limiter) started() {}

func (*limiter) oomKilled() bool { return false }

func (*limiter) release() {}

func ReleaseCgroups() {}

func removePodCgroup(string) {}

func removeSettledPodCgroups(func(string) bool) {}

func oomKilledLimit(string, Holder) uint64 { return 0 }

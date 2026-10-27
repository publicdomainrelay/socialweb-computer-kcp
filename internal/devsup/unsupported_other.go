//go:build !linux && !darwin

package devsup

import (
	"context"
	"fmt"
	"runtime"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

func Run(ctx context.Context, o Options) error {
	return fmt.Errorf("%w: devsup, the proc placement backend, is unsupported on %s; see plan 0011", hostsvc.ErrUnsupported, runtime.GOOS)
}

func SweepOffline(state string) ([]SweepReport, error) {
	return nil, fmt.Errorf("%w: devsup sweep on %s", hostsvc.ErrUnsupported, runtime.GOOS)
}

func InstanceProcesses(instance string) []int { return nil }

func KillInstance(instance string) []int { return nil }

func Alive(pid int) bool { return false }

func Exists(pid int) bool { return false }

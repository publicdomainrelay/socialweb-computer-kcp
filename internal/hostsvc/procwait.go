//go:build linux || darwin

package hostsvc

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
)

func (Processes) Wait(ctx context.Context, pid int, startTicks uint64) error {
	return hostos.AwaitExit(ctx, pid, startTicks)
}

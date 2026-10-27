//go:build darwin

package devsup

import (
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
)

func pin(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("%w: pid %d", errGone, pid)
	}
	ticks, err := hostos.ProcStart(pid)
	if err != nil {
		return 0, fmt.Errorf("%w: pid %d: %v", errGone, pid, err)
	}
	return ticks, nil
}

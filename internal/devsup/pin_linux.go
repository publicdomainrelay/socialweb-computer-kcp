//go:build linux

package devsup

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func pin(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("%w: pid %d", errGone, pid)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return 0, fmt.Errorf("%w: pidfd_open(%d): %v", errGone, pid, err)
	}
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	p, ok := readProcInfo(pid)
	if !ok || p.state == 'Z' || p.state == 'X' {
		return 0, fmt.Errorf("%w: pid %d", errGone, pid)
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return 0, fmt.Errorf("%w: pidfd_send_signal(%d, 0): %v", errGone, pid, err)
	}
	return p.ticks, nil
}

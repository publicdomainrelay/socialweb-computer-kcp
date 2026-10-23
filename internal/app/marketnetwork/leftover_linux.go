package marketnetwork

import (
	"time"

	"golang.org/x/sys/unix"
)

func exits(pids []int) []int {
	var fds []int
	for _, pid := range pids {
		if fd, err := unix.PidfdOpen(pid, 0); err == nil {
			fds = append(fds, fd)
		}
	}
	return fds
}

func awaitExits(fds []int, grace time.Duration) {
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	deadline := time.Now().Add(grace)
	open := append([]int(nil), fds...)
	for len(open) > 0 {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		pfds := make([]unix.PollFd, len(open))
		for i, fd := range open {
			pfds[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
		}
		_, err := unix.Poll(pfds, int(left.Milliseconds())+1)
		if err != nil && err != unix.EINTR {
			return
		}
		var still []int
		for i, p := range pfds {
			if p.Revents == 0 {
				still = append(still, open[i])
			}
		}
		open = still
	}
}

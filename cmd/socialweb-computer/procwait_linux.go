package main

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

func awaitExit(pids []int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var fds []unix.PollFd
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			continue
		}
		fds = append(fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
	}
	defer func() {
		for _, p := range fds {
			unix.Close(int(p.Fd))
		}
	}()
	open := fds
	for len(open) > 0 {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		_, err := unix.Poll(open, int((left+time.Millisecond-1)/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return
		}
		var still []unix.PollFd
		for _, p := range open {
			if p.Revents == 0 {
				still = append(still, unix.PollFd{Fd: p.Fd, Events: unix.POLLIN})
			}
		}
		open = still
	}
}

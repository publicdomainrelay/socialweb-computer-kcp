//go:build linux

package devsup

import "golang.org/x/sys/unix"

func becomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

//go:build linux

package hostos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const statFieldState = 3

const statFieldStartTime = 22

func ProcStart(pid int) (uint64, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
	}
	if err != nil {
		return 0, fmt.Errorf("hostos: reading the stat of pid %d: %w", pid, err)
	}
	state, start, err := parseStat(b)
	if err != nil {
		return 0, err
	}
	if state == 'Z' || state == 'X' {
		return 0, fmt.Errorf("hostos: pid %d has exited (state %c): %w", pid, state, fs.ErrNotExist)
	}
	return start, nil
}

func parseStat(b []byte) (byte, uint64, error) {
	comm := bytes.LastIndexByte(b, ')')
	if comm < 0 {
		return 0, 0, fmt.Errorf("hostos: a /proc/<pid>/stat with no ) closing the comm field: %q",
			string(bytes.TrimSpace(b)))
	}
	fields := strings.Fields(string(b[comm+1:]))
	state := 0
	start := statFieldStartTime - statFieldState
	if len(fields) <= start {
		return 0, 0, fmt.Errorf("hostos: /proc/<pid>/stat carries %d fields after the comm field, "+
			"and the process start time is field %d, so this process cannot be told apart from a "+
			"later one that reuses its pid", len(fields), statFieldStartTime)
	}
	if len(fields[state]) != 1 {
		return 0, 0, fmt.Errorf("hostos: /proc/<pid>/stat names the state %q, which is not one character",
			fields[state])
	}
	n, err := strconv.ParseUint(fields[start], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("hostos: /proc/<pid>/stat carries %q where the process start time "+
			"(field %d) belongs: %w", fields[start], statFieldStartTime, err)
	}
	return fields[state][0], n, nil
}

func Cmdline(pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
	}
	if err != nil {
		return nil, fmt.Errorf("hostos: reading the command line of pid %d: %w", pid, err)
	}
	return strs.SplitNUL(b), nil
}

func Alive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	}
	return false, fmt.Errorf("hostos: probing pid %d: %w", pid, err)
}

func Signal(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("hostos: signalling pid %d: %w", pid, err)
	}
	return nil
}

func Reload(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("hostos: sending SIGHUP to pid %d: %w", pid, err)
	}
	return nil
}

func Terminate(pid int, grace time.Duration) error {
	ticks, err := ProcStart(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := Signal(pid, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err = AwaitExit(ctx, pid, ticks)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return err
	}
	return Signal(pid, true)
}

func AwaitExit(ctx context.Context, pid int, startTicks uint64) error {
	if pid <= 0 {
		return fmt.Errorf("hostos: no process at pid %d to wait for", pid)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hostos: pidfd_open(%d): %w", pid, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return fmt.Errorf("hostos: the pidfd of pid %d: %w", pid, err)
	}
	f := os.NewFile(uintptr(fd), "pidfd:"+strconv.Itoa(pid))
	defer f.Close()
	if startTicks != 0 {
		ticks, err := ProcStart(pid)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if ticks != startTicks {
			return nil
		}
	}
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("hostos: the pidfd of pid %d cannot be polled: %w", pid, err)
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("hostos: the pidfd of pid %d: %w", pid, err)
	}
	stop := context.AfterFunc(ctx, func() { f.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	var perr error
	err = rc.Read(func(fd uintptr) bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		if err != nil && !errors.Is(err, unix.EINTR) {
			perr = err
			return true
		}
		return n > 0
	})
	switch {
	case err == nil && perr == nil:
		return nil
	case err == nil:
		return fmt.Errorf("hostos: polling the pidfd of pid %d: %w", pid, perr)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("hostos: waiting on the pidfd of pid %d: %w", pid, err)
}

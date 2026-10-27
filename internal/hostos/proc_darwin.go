//go:build darwin

package hostos

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	startTicksPerSecond = 1000000

	zombieState = 5

	awaitInterval = 100 * time.Millisecond
)

type kinfo struct {
	pid   int
	ppid  int
	pgid  int
	uid   uint32
	state int8
	ticks uint64
}

func readKinfo(pid int) (kinfo, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	switch {
	case errors.Is(err, unix.ESRCH), errors.Is(err, unix.ENOENT), errors.Is(err, syscall.EINVAL):
		return kinfo{}, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
	case err != nil:
		return kinfo{}, fmt.Errorf("hostos: reading the kinfo of pid %d: %w", pid, err)
	case k.Proc.P_pid != int32(pid):
		return kinfo{}, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
	}
	return kinfo{pid: pid, ppid: int(k.Eproc.Ppid), pgid: int(k.Eproc.Pgid), uid: k.Eproc.Ucred.Uid,
		state: k.Proc.P_stat, ticks: startTicks(k)}, nil
}

func startTicks(k *unix.KinfoProc) uint64 {
	return uint64(k.Proc.P_starttime.Sec)*startTicksPerSecond + uint64(k.Proc.P_starttime.Usec)
}

func ProcStart(pid int) (uint64, error) {
	k, err := readKinfo(pid)
	if err != nil {
		return 0, err
	}
	if k.state == zombieState {
		return 0, fmt.Errorf("hostos: pid %d has exited: %w", pid, fs.ErrNotExist)
	}
	return k.ticks, nil
}

func Cmdline(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	switch {
	case errors.Is(err, unix.ESRCH), errors.Is(err, unix.ENOENT), errors.Is(err, syscall.EINVAL):
		return nil, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
	case err != nil:
		return nil, fmt.Errorf("hostos: reading the argument area of pid %d: %w", pid, err)
	}
	if len(b) < 4 {
		return nil, fmt.Errorf("hostos: the argument area of pid %d is %d bytes, shorter than its argc", pid, len(b))
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, fmt.Errorf("hostos: the argument area of pid %d carries no executable path", pid)
	}
	rest = bytes.TrimLeft(rest[i:], "\x00")
	fields := bytes.Split(rest, []byte{0})
	if argc > len(fields) {
		argc = len(fields)
	}
	out := make([]string, 0, argc)
	for _, f := range fields[:argc] {
		out = append(out, string(f))
	}
	return out, nil
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

func Terminate(ctx context.Context, pid int, grace time.Duration) error {
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
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := AwaitExit(wctx, pid, ticks); err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return fmt.Errorf("hostos: pid %d outlived its %s grace", pid, grace)
	}
	return Signal(pid, true)
}

func AwaitExit(ctx context.Context, pid int, startTicks uint64) error {
	if pid <= 0 {
		return fmt.Errorf("hostos: no process at pid %d to wait for", pid)
	}
	for {
		err := exited(pid, startTicks)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(awaitInterval):
		}
	}
}

func exited(pid int, ticks uint64) error {
	k, err := readKinfo(pid)
	if err != nil {
		return err
	}
	if k.state == zombieState || (ticks != 0 && k.ticks != ticks) {
		return fmt.Errorf("hostos: pid %d has gone: %w", pid, fs.ErrNotExist)
	}
	return nil
}

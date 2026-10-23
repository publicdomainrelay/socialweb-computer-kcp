//go:build linux

package hostos

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	if err := Signal(pid, false); err != nil {
		return err
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		alive, err := Alive(pid)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return Signal(pid, true)
}

//go:build windows

package hostos

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const stillActive = 259

func gone(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_FOUND)
}

func ProcStart(pid int) (uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if gone(err) {
			return 0, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
		}
		return 0, fmt.Errorf("hostos: opening pid %d to read its start time: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		if gone(err) {
			return 0, fmt.Errorf("hostos: no process at pid %d: %w", pid, fs.ErrNotExist)
		}
		return 0, fmt.Errorf("hostos: reading the process times of pid %d: %w", pid, err)
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}

func Cmdline(pid int) ([]string, error) {
	script := fmt.Sprintf("(Get-CimInstance Win32_Process -Filter \"ProcessId=%d\").CommandLine", pid)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return nil, fmt.Errorf("hostos: asking the Windows management interface for the command "+
			"line of pid %d, which is what a node record's identity is matched against: %w", pid, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, fmt.Errorf("hostos: pid %d answered with no command line, so the process a "+
			"record names cannot be matched and nothing may be signalled on its behalf: %w",
			pid, fs.ErrNotExist)
	}
	argv, err := windows.DecomposeCommandLine(line)
	if err != nil {
		return nil, fmt.Errorf("hostos: parsing the command line of pid %d (%q): %w", pid, line, err)
	}
	return argv, nil
}

func Alive(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if gone(err) {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true, nil
		}
		return false, fmt.Errorf("hostos: probing pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false, fmt.Errorf("hostos: reading the exit code of pid %d: %w", pid, err)
	}
	return code == stillActive, nil
}

func Signal(pid int, force bool) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if gone(err) {
			return nil
		}
		return fmt.Errorf("hostos: opening pid %d to stop it: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil && !gone(err) {
		return fmt.Errorf("hostos: terminating pid %d: %w", pid, err)
	}
	return nil
}

func Reload(pid int) error {
	return fmt.Errorf("hostos: pid %d cannot be asked to reload: Windows has no SIGHUP", pid)
}

func Terminate(pid int, grace time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if gone(err) {
			return nil
		}
		return fmt.Errorf("hostos: opening pid %d to wait for it: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := Signal(pid, false); err != nil {
		return err
	}
	ev, err := windows.WaitForSingleObject(handle, uint32(max(0, min(grace.Milliseconds(), int64(windows.INFINITE-1)))))
	if err != nil {
		return fmt.Errorf("hostos: waiting for pid %d: %w", pid, err)
	}
	if ev == windows.WAIT_OBJECT_0 {
		return nil
	}
	return Signal(pid, true)
}

//go:build unix

package guestagent

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

var jobSignals = map[string]syscall.Signal{"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "INT": syscall.SIGINT}

func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalGroup(cmd *exec.Cmd, name string) error {
	sig, ok := jobSignals[name]
	if !ok {
		return fmt.Errorf("signal %q is not TERM, KILL or INT", name)
	}
	if cmd.Process == nil {
		return errors.New("the job has no process")
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

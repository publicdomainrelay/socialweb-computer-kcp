//go:build !unix

package guestagent

import (
	"errors"
	"fmt"
	"os/exec"
)

func setGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, name string) error {
	switch name {
	case "TERM", "KILL", "INT":
	default:
		return fmt.Errorf("signal %q is not TERM, KILL or INT", name)
	}
	if cmd.Process == nil {
		return errors.New("the job has no process")
	}
	return cmd.Process.Kill()
}

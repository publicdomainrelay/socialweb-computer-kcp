//go:build darwin

package hostos

import (
	"os"
	"syscall"
)

// ponytail: darwin has no Pdeathsig, so a child here outlives a killed parent;
// plan 0012 decides the upgrade (a kqueue NOTE_EXIT watcher in the child, or launchd).
func DieWithParent() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func NewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func UnitProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func GracefulSignal() os.Signal { return syscall.SIGTERM }

func ForcedSignal() os.Signal { return syscall.SIGKILL }

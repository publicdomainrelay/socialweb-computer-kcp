//go:build linux

package hostos

import (
	"os"
	"syscall"
)

func DieWithParent() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func NewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func UnitProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func GracefulSignal() os.Signal { return syscall.SIGTERM }

func ForcedSignal() os.Signal { return syscall.SIGKILL }

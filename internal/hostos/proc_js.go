//go:build js

package hostos

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

func DieWithParent() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func NewProcessGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func GracefulSignal() os.Signal { return os.Interrupt }

func ForcedSignal() os.Signal { return os.Kill }

func BootID() (string, error) {
	return "", unsupported("reading the boot id", "0012")
}

func ProcStart(pid int) (uint64, error) {
	return 0, unsupported(fmt.Sprintf("reading the start time of pid %d", pid), "0012")
}

func Alive(pid int) (bool, error) {
	return false, unsupported(fmt.Sprintf("probing pid %d", pid), "0012")
}

func Cmdline(pid int) ([]string, error) {
	return nil, unsupported(fmt.Sprintf("reading the command line of pid %d", pid), "0012")
}

func Signal(pid int, force bool) error {
	return unsupported(fmt.Sprintf("signalling pid %d", pid), "0012")
}

func Terminate(_ context.Context, pid int, grace time.Duration) error {
	return unsupported(fmt.Sprintf("terminating pid %d", pid), "0012")
}

func Reload(pid int) error {
	return unsupported(fmt.Sprintf("asking pid %d to reload", pid), "0012")
}

func LocalPortRange() (int, int, error) {
	return 0, 0, unsupported("reading the local port range", "0012")
}

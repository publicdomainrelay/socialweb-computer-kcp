//go:build darwin

package hostos

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func BootID() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", fmt.Errorf("hostos: reading kern.boottime: %w", err)
	}
	if tv.Sec == 0 {
		return "", fmt.Errorf("hostos: kern.boottime is zero")
	}
	return fmt.Sprintf("boot-%d", tv.Sec), nil
}

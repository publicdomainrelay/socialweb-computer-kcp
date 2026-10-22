//go:build linux

package nodeagent

import "syscall"

func setHostname(name string) error { return syscall.Sethostname([]byte(name)) }

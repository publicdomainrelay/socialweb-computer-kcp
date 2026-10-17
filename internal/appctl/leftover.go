package appctl

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func processesUnder(dir string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	needle := []byte(filepath.Clean(dir) + string(filepath.Separator))
	self := os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, needle) {
			continue
		}
		out = append(out, pid)
	}
	return out
}

func KillUnder(dir string, grace time.Duration) []int {
	pids := processesUnder(dir)
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			p.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(grace)
	for len(processesUnder(dir)) > 0 && time.Now().Before(deadline) {
		time.Sleep(ChildPoll)
	}
	for _, pid := range processesUnder(dir) {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
	return pids
}

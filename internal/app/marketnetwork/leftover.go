package marketnetwork

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
	exited := exits(pids)
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			p.Signal(syscall.SIGTERM)
		}
	}
	awaitExits(exited, grace)
	for _, pid := range processesUnder(dir) {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
	return pids
}

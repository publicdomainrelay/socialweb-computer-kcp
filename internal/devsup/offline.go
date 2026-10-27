//go:build linux || darwin

package devsup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

func SweepOffline(state string) ([]SweepReport, error) {
	entries, err := filepath.Glob(filepath.Join(state, "units", "*.json"))
	if err != nil {
		return nil, err
	}
	var table []ProcEntry
	for _, p := range procTable() {
		table = append(table, ProcEntry{PID: p.pid, PGID: p.pgid, Ticks: p.ticks})
	}
	var out []SweepReport
	for _, path := range entries {
		var rec Record
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &rec)
		}
		if err != nil {
			continue
		}
		v := SweepDecision(rec, table)
		if v == SweepGroup {
			syscall.Kill(-rec.PGID, syscall.SIGKILL)
		}
		if rec.Devsup != "" {
			for pid, a := range attributedTo(rec.Devsup) {
				if a.unit == rec.Spec.Name {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		out = append(out, SweepReport{Unit: rec.Spec.Name, PGID: rec.PGID, Verdict: v.String()})
		os.Remove(path)
	}
	return out, nil
}

func InstanceProcesses(instance string) []int {
	var out []int
	for _, p := range procTable() {
		if procEnv(p.pid, EnvInstance)[EnvInstance] == instance && p.pid != os.Getpid() {
			out = append(out, p.pid)
		}
	}
	return out
}

func KillInstance(instance string) []int {
	pids := InstanceProcesses(instance)
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	return pids
}

func Alive(pid int) bool {
	p, ok := readProcInfo(pid)
	return ok && p.state != 'Z' && p.state != 'X'
}

func Exists(pid int) bool {
	_, ok := readProcInfo(pid)
	return ok
}

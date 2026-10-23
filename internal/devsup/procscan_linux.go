//go:build linux

package devsup

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type procInfo struct {
	pid   int
	ppid  int
	pgid  int
	ticks uint64
	state byte
}

func readProcInfo(pid int) (procInfo, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procInfo{}, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procInfo{}, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 || len(f[0]) != 1 {
		return procInfo{}, false
	}
	ppid, _ := strconv.Atoi(f[1])
	pgid, _ := strconv.Atoi(f[2])
	ticks, _ := strconv.ParseUint(f[19], 10, 64)
	return procInfo{pid: pid, ppid: ppid, pgid: pgid, ticks: ticks, state: f[0][0]}, true
}

func ownPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	uid := uint32(os.Getuid())
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uid {
			continue
		}
		out = append(out, pid)
	}
	return out
}

func procTable() []procInfo {
	var out []procInfo
	for _, pid := range ownPIDs() {
		if p, ok := readProcInfo(pid); ok && p.state != 'Z' && p.state != 'X' {
			out = append(out, p)
		}
	}
	return out
}

func procEnv(pid int, keys ...string) map[string]string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for kv := range bytes.SplitSeq(b, []byte{0}) {
		for _, k := range keys {
			if bytes.HasPrefix(kv, []byte(k+"=")) {
				out[k] = string(kv[len(k)+1:])
			}
		}
	}
	return out
}

func childrenOf(pid int) []int {
	tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
	if err != nil {
		return nil
	}
	var out []int
	for _, t := range tasks {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", t.Name(), "children"))
		if err != nil {
			continue
		}
		for f := range strings.FieldsSeq(string(b)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

type attribution struct {
	unit       string
	invocation string
}

func attributedTo(devsupID string) map[int]attribution {
	out := map[int]attribution{}
	for _, p := range procTable() {
		env := procEnv(p.pid, EnvDevsup, EnvUnit, EnvInvocation)
		if env[EnvDevsup] != devsupID || env[EnvUnit] == "" {
			continue
		}
		out[p.pid] = attribution{unit: env[EnvUnit], invocation: env[EnvInvocation]}
	}
	return out
}

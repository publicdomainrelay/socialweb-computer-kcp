//go:build darwin

package devsup

import (
	"bytes"
	"encoding/binary"
	"os"

	"golang.org/x/sys/unix"
)

const (
	zombieState = 5

	startTicksPerSecond = 1000000
)

type procInfo struct {
	pid    int
	ppid   int
	pgid   int
	ticks  uint64
	uid    uint32
	state  int8
	failed bool
}

func readProcInfo(pid int) (procInfo, bool) {
	if pid <= 0 {
		return procInfo{}, false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return procInfo{}, false
	}
	return infoOf(k), true
}

func infoOf(k *unix.KinfoProc) procInfo {
	return procInfo{pid: int(k.Proc.P_pid), ppid: int(k.Eproc.Ppid), pgid: int(k.Eproc.Pgid),
		ticks: uint64(k.Proc.P_starttime.Sec)*startTicksPerSecond + uint64(k.Proc.P_starttime.Usec),
		uid:   k.Eproc.Ucred.Uid, state: k.Proc.P_stat}
}

func procTable() []procInfo {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	uid := uint32(os.Getuid())
	out := make([]procInfo, 0, len(all))
	for i := range all {
		p := infoOf(&all[i])
		if p.uid != uid || p.state == zombieState || p.pid <= 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

func ownPIDs() []int {
	var out []int
	for _, p := range procTable() {
		out = append(out, p.pid)
	}
	return out
}

func procEnv(pid int, keys ...string) map[string]string {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil
	}
	rest = bytes.TrimLeft(rest[i:], "\x00")
	for range argc {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return nil
		}
		rest = rest[j+1:]
	}
	out := map[string]string{}
	for kv := range bytes.SplitSeq(rest, []byte{0}) {
		for _, k := range keys {
			if bytes.HasPrefix(kv, []byte(k+"=")) {
				out[k] = string(kv[len(k)+1:])
			}
		}
	}
	return out
}

func childrenOf(pid int) []int {
	var out []int
	for _, p := range procTable() {
		if p.ppid == pid {
			out = append(out, p.pid)
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

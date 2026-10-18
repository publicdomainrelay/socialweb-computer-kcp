//go:build linux

package execrunner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	cgroupMount = "/sys/fs/cgroup"

	supervisorLeaf = "supervisor"

	podCgroupPrefix = "pod-"

	subtreeAttempts = 20

	subtreeRetryDelay = 50 * time.Millisecond

	removeAttempts = 40

	removeRetryDelay = 25 * time.Millisecond

	podCgroupSettle = time.Minute

	runIDTimeLayout = "20060102T150405.000000000"
)

type cgroupBase struct {
	mu   sync.Mutex
	path string
	leaf string
	held bool
}

var podBase cgroupBase

type limiter struct {
	dir string
	fd  *os.File
}

func cgroupDirOf(pid string) (string, error) {
	body, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			rest = strings.TrimSuffix(strings.TrimSpace(rest), " (deleted)")
			dir := filepath.Join(cgroupMount, rest)
			if filepath.Base(dir) == supervisorLeaf {
				dir = filepath.Dir(dir)
			}
			return dir, nil
		}
	}
	return "", errors.New("the process is in no cgroup v2 hierarchy")
}

func podCgroupBase(holder Holder) (string, error) {
	podBase.mu.Lock()
	defer podBase.mu.Unlock()
	pid := 0
	if holder != nil {
		var err error
		if pid, err = holder(); err != nil {
			return "", fmt.Errorf("execrunner: the unit that holds the runs: %w", err)
		}
		dir, err := cgroupDirOf(strconv.Itoa(pid))
		if err != nil {
			return "", fmt.Errorf("execrunner: the cgroup of the unit that holds the runs: %w", err)
		}
		if podBase.held && podBase.path == dir {
			return dir, nil
		}
	} else if podBase.path != "" && !podBase.held {
		return podBase.path, nil
	}
	path, leaf, err := prepareBase(pid)
	if err != nil {
		return "", fmt.Errorf("execrunner: a pod cgroup needs a cgroup this process may subdivide (a unit with Delegate=yes): %w", err)
	}
	podBase.path, podBase.leaf, podBase.held = path, leaf, holder != nil
	return path, nil
}

func removePodCgroup(id string) {
	podBase.mu.Lock()
	base := podBase.path
	podBase.mu.Unlock()
	if base != "" {
		_ = os.Remove(filepath.Join(base, podCgroupPrefix+id))
	}
}

func ReleaseCgroups() {
	podBase.mu.Lock()
	defer podBase.mu.Unlock()
	base, leaf, held := podBase.path, podBase.leaf, podBase.held
	if base == "" {
		return
	}
	podBase.path, podBase.leaf, podBase.held = "", "", false
	if held {
		return
	}
	killPodCgroups(base)
	_ = os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("-memory"), 0o644)
	_ = os.WriteFile(filepath.Join(base, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	_ = movePids(leaf, base)
	_ = os.Remove(leaf)
}

func movePids(from, to string) error {
	body, err := os.ReadFile(filepath.Join(from, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(string(body)) {
		if err := os.WriteFile(filepath.Join(to, "cgroup.procs"), []byte(field), 0o644); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func prepareBase(holderPID int) (string, string, error) {
	who := "self"
	if holderPID > 0 {
		who = strconv.Itoa(holderPID)
	}
	dir, err := cgroupDirOf(who)
	if err != nil {
		return "", "", err
	}
	base, leaf := dir, filepath.Join(dir, supervisorLeaf)
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	if holderPID == 0 {
		if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			return "", "", err
		}
	}
	for attempt := 0; ; attempt++ {
		if err := movePids(base, leaf); err != nil {
			return "", "", err
		}
		err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+memory"), 0o644)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EBUSY) || attempt >= subtreeAttempts {
			return "", "", err
		}
		time.Sleep(subtreeRetryDelay)
	}
	removeIdlePodCgroups(base)
	return base, leaf, nil
}

func removeIdlePodCgroups(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), podCgroupPrefix) {
			_ = os.Remove(filepath.Join(base, e.Name()))
		}
	}
}

func killPodCgroups(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), podCgroupPrefix) {
			killAndRemove(filepath.Join(base, e.Name()))
		}
	}
}

func killAndRemove(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
	for attempt := 0; attempt < removeAttempts; attempt++ {
		if err := os.Remove(dir); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(removeRetryDelay)
	}
}

func removeSettledPodCgroups(named func(string) bool) {
	podBase.mu.Lock()
	base := podBase.path
	podBase.mu.Unlock()
	if base == "" {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	settled := time.Now().Add(-podCgroupSettle)
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Name(), podCgroupPrefix)
		if !e.IsDir() || !ok || !IsRunID(id) || named(id) {
			continue
		}
		stamp := id[strings.Index(id, "-")+1 : strings.LastIndex(id, "-")]
		if at, err := time.Parse(runIDTimeLayout, stamp); err != nil || at.After(settled) {
			continue
		}
		_ = os.Remove(filepath.Join(base, e.Name()))
	}
}

func newLimiter(cmd *exec.Cmd, id string, memoryMax uint64, holder Holder) (*limiter, error) {
	base, err := podCgroupBase(holder)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, podCgroupPrefix+id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("execrunner: create the pod cgroup: %w", err)
	}
	l := &limiter{dir: dir}
	limits := map[string]string{}
	if memoryMax > 0 {
		limits = map[string]string{
			"memory.max":       strconv.FormatUint(memoryMax, 10),
			"memory.swap.max":  "0",
			"memory.oom.group": "1",
		}
	}
	for name, value := range limits {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			l.release()
			return nil, fmt.Errorf("execrunner: set %s on the pod cgroup: %w", name, err)
		}
	}
	fd, err := os.Open(dir)
	if err != nil {
		l.release()
		return nil, fmt.Errorf("execrunner: open the pod cgroup: %w", err)
	}
	l.fd = fd
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())
	return l, nil
}

func (l *limiter) started() {
	if l != nil && l.fd != nil {
		_ = l.fd.Close()
		l.fd = nil
	}
}

func (l *limiter) oomKilled() bool {
	if l == nil {
		return false
	}
	body, err := os.ReadFile(filepath.Join(l.dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
			return n > 0
		}
	}
	return false
}

func oomKilledLimit(id string, holder Holder) uint64 {
	podBase.mu.Lock()
	base := podBase.path
	podBase.mu.Unlock()
	if base == "" && holder != nil {
		if pid, err := holder(); err == nil {
			base, _ = cgroupDirOf(strconv.Itoa(pid))
		}
	}
	if base == "" {
		return 0
	}
	l := &limiter{dir: filepath.Join(base, podCgroupPrefix+id)}
	if !l.oomKilled() {
		return 0
	}
	body, err := os.ReadFile(filepath.Join(l.dir, "memory.max"))
	if err != nil {
		return 0
	}
	limit, _ := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
	return limit
}

func (l *limiter) release() {
	if l == nil {
		return
	}
	l.started()
	killAndRemove(l.dir)
}

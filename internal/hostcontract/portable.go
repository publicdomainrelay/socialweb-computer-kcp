package hostcontract

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func GroupMembership(content, group, account string) (int, bool) {
	for line := range strings.SplitSeq(content, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) != 4 || f[0] != group {
			continue
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			return -1, false
		}
		for m := range strings.SplitSeq(f[3], ",") {
			if m == account && account != "" {
				return gid, true
			}
		}
		return gid, false
	}
	return -1, false
}

func ManagerGroups(root string, uid int) (int, []int, string) {
	procs := filepath.Join(root, "sys/fs/cgroup/user.slice", fmt.Sprintf("user-%d.slice", uid), fmt.Sprintf("user@%d.service", uid), "init.scope", "cgroup.procs")
	b, err := os.ReadFile(procs)
	if err != nil {
		return 0, nil, "user@" + strconv.Itoa(uid) + " is not running: " + err.Error()
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, nil, "user@" + strconv.Itoa(uid) + " has no process in init.scope"
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, nil, err.Error()
	}
	status, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return pid, nil, err.Error()
	}
	return pid, ParseGroups(string(status)), ""
}

func ParseGroups(status string) []int {
	sc := bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || k != "Groups" {
			continue
		}
		var out []int
		for g := range strings.FieldsSeq(v) {
			if n, err := strconv.Atoi(g); err == nil {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

func AppArmorProfileLoaded(profiles, name string) bool {
	for line := range strings.SplitSeq(profiles, "\n") {
		if strings.HasPrefix(line, name+" ") || line == name {
			return true
		}
	}
	return false
}

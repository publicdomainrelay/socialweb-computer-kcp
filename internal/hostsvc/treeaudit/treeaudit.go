package treeaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	VMPrefix            = hostsvc.NamePrefix + "vm-"
	MarketNetworkPrefix = hostsvc.NamePrefix + "market-network-"
	MarketNetworkVMPart = "-vm-"
	EnvUnit             = "SWC_UNIT"
	EnvInvoc            = "INVOCATION_ID"
)

var VMAssets = []string{"firecracker", "pasta"}

type Proc struct {
	PID    int    `json:"pid"`
	Exe    string `json:"exe,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Err    string `json:"err,omitempty"`
}

type Unit struct {
	Name    string              `json:"name"`
	State   hostsvc.ActiveState `json:"state"`
	MainPID int                 `json:"mainPID"`
	Procs   []Proc              `json:"procs"`
	Err     string              `json:"err,omitempty"`
}

type Finding struct {
	Unit   string `json:"unit"`
	PID    int    `json:"pid,omitempty"`
	Exe    string `json:"exe,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Why    string `json:"why"`
}

type Report struct {
	Backend  hostsvc.Backend `json:"backend"`
	Units    []Unit          `json:"units"`
	Findings []Finding       `json:"findings"`
}

func (r Report) Pass() bool { return len(r.Findings) == 0 }

func IsVMUnit(name string) bool {
	if strings.HasPrefix(name, VMPrefix) {
		return true
	}
	return strings.HasPrefix(name, MarketNetworkPrefix) && strings.Contains(strings.TrimPrefix(name, MarketNetworkPrefix), MarketNetworkVMPart)
}

func Judge(units []Unit, pinned map[string]string) []Finding {
	var out []Finding
	for _, u := range units {
		switch {
		case u.Err != "":
			out = append(out, Finding{Unit: u.Name, Why: "its process tree could not be read: " + u.Err})
			continue
		case u.State != hostsvc.Active:
			out = append(out, Finding{Unit: u.Name, Why: fmt.Sprintf("the unit is %s, not active: a VM unit passes only once it runs nothing but pinned assets", u.State)})
			continue
		case len(u.Procs) == 0:
			out = append(out, Finding{Unit: u.Name, Why: "the active unit has no process"})
			continue
		}
		for _, p := range u.Procs {
			switch {
			case p.Err != "":
				out = append(out, Finding{Unit: u.Name, PID: p.PID, Exe: p.Exe, Why: "the executable could not be hashed: " + p.Err})
			case pinned[p.SHA256] == "":
				out = append(out, Finding{Unit: u.Name, PID: p.PID, Exe: p.Exe, SHA256: p.SHA256,
					Why: "not a pinned VM-unit asset (" + strings.Join(VMAssets, ", ") + "): no socialweb-computer code, vmexec or trampoline shell may stay in a running VM's tree"})
			}
		}
	}
	return out
}

type Host struct {
	ProcRoot   string
	CgroupRoot string
}

func (h Host) proc() string {
	if h.ProcRoot == "" {
		return "/proc"
	}
	return h.ProcRoot
}

func (h Host) cgroup() string {
	if h.CgroupRoot == "" {
		return "/sys/fs/cgroup"
	}
	return h.CgroupRoot
}

func (h Host) CgroupMembers(mainPID int, unit string) ([]int, error) {
	if mainPID <= 0 {
		return nil, fmt.Errorf("%s has no main pid", unit)
	}
	b, err := os.ReadFile(filepath.Join(h.proc(), strconv.Itoa(mainPID), "cgroup"))
	if err != nil {
		return nil, err
	}
	var path string
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			path = rest
		}
	}
	if path == "" {
		return nil, fmt.Errorf("pid %d has no cgroup v2 path", mainPID)
	}
	for filepath.Base(path) != unit {
		if path == "/" || path == "." {
			return nil, fmt.Errorf("the main pid %d of %s is not inside the unit's cgroup", mainPID, unit)
		}
		path = filepath.Dir(path)
	}
	var pids []int
	err = filepath.WalkDir(filepath.Join(h.cgroup(), path), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for f := range strings.FieldsSeq(string(raw)) {
			if n, err := strconv.Atoi(f); err == nil {
				pids = append(pids, n)
			}
		}
		return nil
	})
	sort.Ints(pids)
	return pids, err
}

func statPGID(b []byte) (int, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 3 || f[0] == "Z" || f[0] == "X" {
		return 0, false
	}
	n, err := strconv.Atoi(f[2])
	return n, err == nil
}

func environ(b []byte) map[string]string {
	out := map[string]string{}
	for kv := range bytes.SplitSeq(b, []byte{0}) {
		k, v, ok := strings.Cut(string(kv), "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func (h Host) ProcMembers(leader int, unit, invocation string, devsup int) ([]int, error) {
	entries, err := os.ReadDir(h.proc())
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == devsup {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(h.proc(), e.Name(), "stat"))
		if err != nil {
			continue
		}
		pgid, alive := statPGID(stat)
		if !alive {
			continue
		}
		if leader > 0 && pgid == leader {
			pids = append(pids, pid)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(h.proc(), e.Name(), "environ"))
		if err != nil {
			continue
		}
		env := environ(raw)
		if env[EnvUnit] == unit && (invocation == "" || env[EnvInvoc] == invocation) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func (h Host) Hash(pid int) Proc {
	p := Proc{PID: pid}
	link := filepath.Join(h.proc(), strconv.Itoa(pid), "exe")
	if target, err := os.Readlink(link); err == nil {
		p.Exe = target
	}
	f, err := os.Open(link)
	if err != nil {
		p.Err = err.Error()
		return p
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		p.Err = err.Error()
		return p
	}
	p.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return p
}

type Lister interface {
	List(ctx context.Context, prefix string) ([]hostsvc.UnitStatus, error)
	Capabilities() hostsvc.Capabilities
}

func Collect(ctx context.Context, m Lister, h Host, devsup int) (Report, error) {
	r := Report{Backend: m.Capabilities().Backend}
	var statuses []hostsvc.UnitStatus
	for _, prefix := range []string{VMPrefix, MarketNetworkPrefix} {
		st, err := m.List(ctx, prefix)
		if err != nil {
			return r, err
		}
		statuses = append(statuses, st...)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	for _, st := range statuses {
		if !IsVMUnit(st.Name) {
			continue
		}
		u := Unit{Name: st.Name, State: st.State, MainPID: st.MainPID}
		var pids []int
		var err error
		switch r.Backend {
		case hostsvc.BackendSystemdUser:
			pids, err = h.CgroupMembers(st.MainPID, st.Name)
		case hostsvc.BackendProc:
			pids, err = h.ProcMembers(st.MainPID, st.Name, st.InvocationID, devsup)
		default:
			err = fmt.Errorf("no tree audit for backend %q", r.Backend)
		}
		if err != nil && st.State == hostsvc.Active {
			u.Err = err.Error()
		}
		for _, pid := range pids {
			u.Procs = append(u.Procs, h.Hash(pid))
		}
		r.Units = append(r.Units, u)
	}
	return r, nil
}

func Audit(ctx context.Context, m Lister, h Host, devsup int, pinned map[string]string) (Report, error) {
	r, err := Collect(ctx, m, h, devsup)
	if err != nil {
		return r, err
	}
	r.Findings = Judge(r.Units, pinned)
	return r, nil
}

var digestDir = regexp.MustCompile(`^[0-9a-f]{64}$`)

type TreeDigest func(dir string) (string, error)

func Pinned(runtimeRoot string, pins []versions.Pin, digest TreeDigest) (map[string]string, []string) {
	out := map[string]string{}
	var notes []string
	execNames := map[string]string{}
	trees := map[string]string{}
	for _, p := range pins {
		if !isVMAsset(p.Name) {
			continue
		}
		for _, rel := range p.Executables {
			execNames[rel] = p.Name
		}
		if p.TreeSHA256 != "" {
			trees[p.TreeSHA256] = p.Name
		}
	}
	entries, err := os.ReadDir(runtimeRoot)
	if err != nil {
		return out, []string{"no runtime set under " + runtimeRoot + ": " + err.Error()}
	}
	for _, e := range entries {
		if !e.IsDir() || !digestDir.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(runtimeRoot, e.Name())
		var found []string
		for rel := range execNames {
			if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
				found = append(found, rel)
			}
		}
		if len(found) == 0 {
			continue
		}
		if got, err := digest(dir); err != nil || got != e.Name() {
			notes = append(notes, fmt.Sprintf("%s does not verify against its name (%v); its executables are not pinned", dir, err))
			continue
		}
		asset, whole := trees[e.Name()]
		if !whole {
			for _, rel := range found {
				sum, err := fileSHA256(filepath.Join(dir, rel))
				if err != nil {
					notes = append(notes, err.Error())
					continue
				}
				out[sum] = execNames[rel] + " " + filepath.Join(dir, rel)
			}
			continue
		}
		werr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			info, err := d.Info()
			if err != nil || info.Mode().Perm()&0o111 == 0 {
				return err
			}
			sum, err := fileSHA256(p)
			if err != nil {
				return err
			}
			out[sum] = asset + " " + p
			return nil
		})
		if werr != nil {
			notes = append(notes, werr.Error())
		}
	}
	return out, notes
}

func isVMAsset(name string) bool {
	return slices.Contains(VMAssets, name)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func Write(w io.Writer, r Report) {
	fmt.Fprintf(w, "backend %s: %d VM unit(s)\n", r.Backend, len(r.Units))
	for _, u := range r.Units {
		fmt.Fprintf(w, "  %s %s main pid %d, %d process(es)\n", u.Name, u.State, u.MainPID, len(u.Procs))
	}
	for _, f := range r.Findings {
		fmt.Fprintf(w, "FAIL %s pid %d exe %s: %s\n", f.Unit, f.PID, f.Exe, f.Why)
	}
}

package devcheck

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

const (
	CheckNotRoot        = "not-root"
	CheckRuntimeDir     = "xdg-runtime-dir"
	CheckKVM            = "dev-kvm"
	CheckTun            = "dev-net-tun"
	CheckUserns         = "unprivileged-userns"
	CheckAppArmorUserns = "apparmor-userns-restriction"
	CheckNewuidmap      = "newuidmap-newgidmap"
	CheckSubuid         = "subuid-subgid"
	CheckLogindKill     = "logind-kill-user-processes"
)

var Names = []string{CheckNotRoot, CheckRuntimeDir, CheckKVM, CheckTun, CheckUserns, CheckAppArmorUserns,
	CheckNewuidmap, CheckSubuid, CheckLogindKill}

type Env interface {
	LookupEnv(key string) (string, bool)
}

type Host struct {
	Root     string
	Euid     int
	User     string
	Env      Env
	LookPath func(name string) (string, error)
}

type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Needed string `json:"neededBy"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (h Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h Host) read(p string) (string, bool) {
	b, err := os.ReadFile(h.path(p))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func (h Host) readWrite(p string) error {
	f, err := os.OpenFile(h.path(p), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

func Run(h Host) []Result {
	return []Result{
		notRoot(h), runtimeDir(h), device(h, CheckKVM, "/dev/kvm", "VM guests (firecracker)",
			"add the user to the kvm group (root, once) and log in again"),
		device(h, CheckTun, "/dev/net/tun", "guest networking (pasta)",
			"load the tun module (root): modprobe tun"),
		userns(h), apparmor(h), newuidmap(h), subuid(h), logind(h),
	}
}

func notRoot(h Host) Result {
	r := Result{Name: CheckNotRoot, Needed: "every dev verb", Status: StatusOK, Detail: "euid " + strconv.Itoa(h.Euid)}
	if h.Euid == 0 {
		r.Status, r.Fix = StatusFail, "run dev as your own user; dev mode needs no root and production uses install"
	}
	return r
}

func runtimeDir(h Host) Result {
	r := Result{Name: CheckRuntimeDir, Needed: "devsup sockets", Status: StatusOK}
	v, ok := h.Env.LookupEnv("XDG_RUNTIME_DIR")
	r.Detail = v
	if !ok || v == "" {
		r.Status, r.Detail = StatusFail, "XDG_RUNTIME_DIR is unset"
		r.Fix = "run from a login session (or enable linger) so /run/user/<uid> exists and XDG_RUNTIME_DIR is set"
	}
	return r
}

func device(h Host, name, dev, needed, fix string) Result {
	r := Result{Name: name, Needed: needed, Status: StatusOK, Detail: dev + " is readable and writable"}
	if err := h.readWrite(dev); err != nil {
		r.Status, r.Detail, r.Fix = StatusFail, err.Error(), fix
	}
	return r
}

func userns(h Host) Result {
	r := Result{Name: CheckUserns, Needed: "pasta and per-VM user namespaces", Status: StatusOK}
	maxNS, ok := h.read("/proc/sys/user/max_user_namespaces")
	n, err := strconv.Atoi(maxNS)
	if !ok || err != nil || n <= 0 {
		r.Status, r.Detail = StatusFail, "user.max_user_namespaces is "+maxNS
		r.Fix = "sysctl user.max_user_namespaces to a positive value (root)"
		return r
	}
	r.Detail = "user.max_user_namespaces=" + maxNS
	if v, ok := h.read("/proc/sys/kernel/unprivileged_userns_clone"); ok && v != "1" {
		r.Status, r.Detail = StatusFail, "kernel.unprivileged_userns_clone="+v
		r.Fix = "sysctl kernel.unprivileged_userns_clone=1 (root)"
	}
	return r
}

func apparmor(h Host) Result {
	r := Result{Name: CheckAppArmorUserns, Needed: "pasta (Ubuntu 24.04+)", Status: StatusOK}
	v, ok := h.read("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	switch {
	case !ok:
		r.Detail = "no AppArmor userns restriction on this kernel"
	case v == "1":
		r.Status, r.Detail = StatusWarn, "kernel.apparmor_restrict_unprivileged_userns=1 (dev reports it; AppArmor in dev is report-only)"
		r.Fix = "install the AppArmor profile granting userns to the pinned pasta path (install does it in production), or sysctl the restriction off (root)"
	default:
		r.Detail = "kernel.apparmor_restrict_unprivileged_userns=" + v
	}
	return r
}

func newuidmap(h Host) Result {
	r := Result{Name: CheckNewuidmap, Needed: "per-VM subuid user namespaces", Status: StatusOK}
	var found, missing []string
	for _, n := range []string{"newuidmap", "newgidmap"} {
		p, err := h.LookPath(n)
		if err != nil {
			missing = append(missing, n)
			continue
		}
		found = append(found, p)
	}
	r.Detail = strings.Join(found, " ")
	if len(missing) > 0 {
		r.Status, r.Detail = StatusFail, "not found: "+strings.Join(missing, " ")
		r.Fix = "install the uidmap (Debian/Ubuntu) or shadow-utils (Fedora) package (root)"
	}
	return r
}

func hasRange(content, user string, uid int) bool {
	for line := range strings.SplitSeq(content, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) == 3 && (f[0] == user || f[0] == strconv.Itoa(uid)) {
			return true
		}
	}
	return false
}

func subuid(h Host) Result {
	r := Result{Name: CheckSubuid, Needed: "per-VM subuid user namespaces", Status: StatusOK}
	var missing []string
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		c, _ := h.read(f)
		if !hasRange(c, h.User, h.Euid) {
			missing = append(missing, f)
		}
	}
	r.Detail = "a range for " + h.User + " in /etc/subuid and /etc/subgid"
	if len(missing) > 0 {
		r.Status, r.Detail = StatusFail, "no range for "+h.User+" in "+strings.Join(missing, " ")
		r.Fix = fmt.Sprintf("usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s (root)", h.User)
	}
	return r
}

func logindSetting(h Host) (string, string) {
	files := []string{"/usr/lib/systemd/logind.conf", "/etc/systemd/logind.conf"}
	for _, dir := range []string{"/usr/lib/systemd/logind.conf.d", "/etc/systemd/logind.conf.d"} {
		matches, _ := filepath.Glob(filepath.Join(h.path(dir), "*.conf"))
		sort.Strings(matches)
		for _, m := range matches {
			rel, _ := filepath.Rel(h.Root, m)
			files = append(files, "/"+rel)
		}
	}
	value, from := "", ""
	for _, f := range files {
		fh, err := os.Open(h.path(f))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if ok && strings.TrimSpace(k) == "KillUserProcesses" {
				value, from = strings.TrimSpace(v), f
			}
		}
		fh.Close()
	}
	return value, from
}

func logind(h Host) Result {
	r := Result{Name: CheckLogindKill, Needed: "a detached devsup surviving logout", Status: StatusOK}
	v, from := logindSetting(h)
	switch strings.ToLower(v) {
	case "yes", "true", "1", "on":
		r.Status, r.Detail = StatusWarn, "KillUserProcesses="+v+" in "+from+": a detached devsup dies at logout"
		r.Fix = "use dev up --fg, or set KillUserProcesses=no in logind.conf (root)"
	case "":
		r.Detail = "KillUserProcesses unset (compiled default no on this distribution family)"
	default:
		r.Detail = "KillUserProcesses=" + v + " in " + from
	}
	return r
}

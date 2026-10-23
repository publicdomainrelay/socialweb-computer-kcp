//go:build linux

package hostcontract

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

type Options struct {
	Role       Role
	StateDir   string
	Self       []string
	PastaPath  string
	AssetCache string
	Getenv     func(string) string
}

const (
	ProbeHoldFlag   = "-probe-hold"
	ProbeUsernsFlag = "-probe-userns"
	childTimeout    = 20 * time.Second
)

func Gather(ctx context.Context, o Options) Facts {
	uid := os.Getuid()
	f := Facts{Role: o.Role, UID: uid, Euid: os.Geteuid(), StateDir: o.StateDir}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		f.Account = u.Username
	}
	f.RuntimeDir = o.Getenv("XDG_RUNTIME_DIR")
	if f.RuntimeDir == "" {
		f.RuntimeDir = filepath.Join("/run/user", strconv.Itoa(uid))
	}
	socket := filepath.Join(f.RuntimeDir, "systemd", "private")
	if v, err := hostsvc.ManagerVersion(ctx, socket); err != nil {
		f.VersionErr = err.Error()
	} else {
		f.SystemdVersion = v
		gatherProbe(ctx, &f, o, socket)
	}
	f.CgroupV2 = cgroup2()
	mgr := filepath.Join("/sys/fs/cgroup/user.slice", fmt.Sprintf("user-%d.slice", uid), fmt.Sprintf("user@%d.service", uid))
	if b, err := os.ReadFile(filepath.Join(mgr, "cgroup.controllers")); err != nil {
		f.ManagerControllersErr = err.Error()
	} else {
		f.ManagerControllers = strings.Fields(string(b))
	}
	_, err := os.Stat(filepath.Join(o.Getenv(StagedRootEnv), "/var/lib/systemd/linger", f.Account))
	f.Linger = err == nil && f.Account != ""
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		f.BootID = strings.TrimSpace(string(b))
	}
	f.Tzdata = fsx.Exists("/usr/share/zoneinfo/UTC")
	f.OomdActive = nonEmpty("/sys/fs/cgroup/system.slice/systemd-oomd.service/cgroup.procs")
	if o.Role != RoleBidder {
		return f
	}
	f.KVMMissing = !fsx.Exists("/dev/kvm")
	f.KVMErr = openRW("/dev/kvm")
	f.TunErr = openRW("/dev/net/tun")
	f.KVMGid, f.AccountInKVM = groupMembership("/etc/group", "kvm", f.Account)
	f.ManagerPID, f.ManagerGroups, f.ManagerErr = ManagerGroups("/", uid)
	if b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil {
		f.AppArmorRestricts = strings.TrimSpace(string(b)) == "1"
	}
	if f.AppArmorRestricts {
		b, err := os.ReadFile("/sys/kernel/security/apparmor/profiles")
		if err != nil {
			f.AppArmorErr = err.Error()
		}
		f.AppArmorLoaded = AppArmorProfileLoaded(string(b), AppArmorProfileName)
	}
	sub, _ := os.ReadFile("/etc/subuid")
	sg, _ := os.ReadFile("/etc/subgid")
	f.Subuid, f.Subgid = string(sub), string(sg)
	f.Helpers = helpers()
	gatherUIDMap(ctx, &f, o)
	gatherPasta(ctx, &f, o)
	f.ACLChecked, f.ACLErr = aclSupported(o.StateDir)
	return f
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func gatherProbe(ctx context.Context, f *Facts, o Options, socket string) {
	if len(o.Self) == 0 || o.StateDir == "" {
		f.ProbeErr = "no executable or state dir to probe with"
		return
	}
	suffix := randomSuffix()
	dir := filepath.Join(o.StateDir, "probe", suffix)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.ProbeErr = err.Error()
		return
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "probe.log")
	spec, extra := ProbeSpec(o.Role, suffix, append(append([]string(nil), o.Self...), ProbeHoldFlag), out)
	r, err := hostsvc.Probe(ctx, socket, spec, extra)
	f.ProbeSent, f.ProbeReadBack = r.Sent, r.ReadBack
	if err != nil {
		f.ProbeErr = err.Error()
	} else if r.Result != "done" {
		f.ProbeErr = "the start job ended " + r.Result
	}
}

func cgroup2() bool {
	var st unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &st); err != nil {
		return false
	}
	return st.Type == unix.CGROUP2_SUPER_MAGIC
}

func nonEmpty(p string) bool {
	b, err := os.ReadFile(p)
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

func openRW(p string) string {
	fh, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err.Error()
	}
	fh.Close()
	return ""
}

func groupMembership(path, group, account string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1, false
	}
	return GroupMembership(string(b), group, account)
}

func helpers() []Helper {
	up, uerr := exec.LookPath("newuidmap")
	gp, gerr := exec.LookPath("newgidmap")
	return []Helper{helperAt("newuidmap", up, uerr), helperAt("newgidmap", gp, gerr)}
}

func helperAt(name, p string, err error) Helper {
	h := Helper{Name: name}
	if err != nil {
		h.Err = "not found on PATH"
		return h
	}
	h.Path = p
	st, err := os.Stat(p)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	h.Setuid = ok && sys.Uid == 0 && st.Mode()&os.ModeSetuid != 0
	if n, err := unix.Getxattr(p, "security.capability", nil); err == nil && n > 0 {
		h.Caps = true
	}
	return h
}

func aclSupported(dir string) (bool, string) {
	if dir == "" {
		return false, "no state dir given"
	}
	_, err := unix.Getxattr(dir, "system.posix_acl_access", nil)
	switch {
	case err == nil, errors.Is(err, unix.ENODATA):
		return true, ""
	case errors.Is(err, unix.ENOTSUP):
		return true, "the filesystem does not support POSIX ACLs"
	}
	return true, err.Error()
}

func gatherPasta(ctx context.Context, f *Facts, o Options) {
	paths := map[string]string{}
	if o.PastaPath != "" {
		paths["pasta"] = o.PastaPath
	}
	e, err := assets.Resolve(assets.Options{Cache: o.AssetCache, Paths: paths}, "pasta")
	if err != nil {
		f.PastaErr = err.Error()
		return
	}
	f.PastaChecked = true
	if len(o.Self) == 0 {
		f.PastaErr = "no executable to exec inside the namespace"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, childTimeout)
	defer cancel()
	var quoted []string
	for _, a := range append(append([]string(nil), o.Self...), "-probe-exec") {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	inner := strings.Join(quoted, " ")
	cmd := exec.CommandContext(ctx, e.Path, "--quiet", "--", "/bin/sh", "-c", "exec "+inner)
	cmd.Env = append(os.Environ(), e.Env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		f.PastaErr = fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))
	}
}

func gatherUIDMap(ctx context.Context, f *Facts, o Options) {
	if len(SubidProblems(f.Subuid, f.Subgid, f.Account, f.UID)) > 0 {
		f.UIDMapErr = "the account has no usable subuid/subgid range"
		return
	}
	for _, h := range f.Helpers {
		if h.Err != "" {
			f.UIDMapErr = h.Name + " " + h.Err
			return
		}
	}
	if len(o.Self) == 0 {
		f.UIDMapErr = "no executable to run as the namespace's init"
		return
	}
	f.UIDMapChecked = true
	ur, _ := AccountRange(f.Subuid, f.Account, f.UID)
	gr, _ := AccountRange(f.Subgid, f.Account, f.UID)
	f.UIDMapErr = MapThrowaway(ctx, o.Self, f.Helpers[0].Path, f.Helpers[1].Path, ur.Start, gr.Start)
}

func MapThrowaway(ctx context.Context, self []string, newuidmap, newgidmap string, uidStart, gidStart uint64) string {
	ctx, cancel := context.WithTimeout(ctx, childTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self[0], append(append([]string(nil), self[1:]...), ProbeUsernsFlag)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER, Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err.Error()
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "starting a child in a new user namespace: " + err.Error()
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	count := strconv.Itoa(VMSlotIDs)
	var problems []string
	for _, m := range []struct{ helper, start string }{{newuidmap, strconv.FormatUint(uidStart, 10)}, {newgidmap, strconv.FormatUint(gidStart, 10)}} {
		if b, err := exec.CommandContext(ctx, m.helper, pid, "0", m.start, count).CombinedOutput(); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v: %s", filepath.Base(m.helper), err, strings.TrimSpace(string(b))))
		}
	}
	_, _ = io.WriteString(stdin, "\n")
	stdin.Close()
	werr := cmd.Wait()
	if len(problems) > 0 {
		return strings.Join(problems, "; ")
	}
	if werr != nil {
		return fmt.Sprintf("the namespace child: %v: %s", werr, strings.TrimSpace(out.String()))
	}
	want := []string{"0 " + strconv.FormatUint(uidStart, 10) + " " + count, "0 " + strconv.FormatUint(gidStart, 10) + " " + count}
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, w := range want {
		if !strings.Contains(got, w) {
			return "the child's maps read " + got + ", want " + strings.Join(want, " and ")
		}
	}
	return ""
}

func UsernsChild(stdin io.Reader, stdout io.Writer) int {
	_, _ = bufio.NewReader(stdin).ReadString('\n')
	for _, p := range []string{"/proc/self/uid_map", "/proc/self/gid_map"} {
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintln(stdout, err)
			return 1
		}
		_, _ = stdout.Write(b)
	}
	return 0
}

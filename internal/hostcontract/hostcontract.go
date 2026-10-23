package hostcontract

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

type Role string

const (
	RoleGateway Role = "gateway"
	RoleBidder  Role = "bidder"
)

const StagedRootEnv = "SOCIALWEB_STAGED_ROOT"

func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleGateway, RoleBidder:
		return Role(s), nil
	}
	return "", fmt.Errorf("hostcontract: role %q: want %s or %s", s, RoleGateway, RoleBidder)
}

type Status string

const (
	StatusOK   Status = "ok"
	StatusFail Status = "fail"
	StatusInfo Status = "info"
)

const (
	CheckAccount           = "account"
	CheckSystemdVersion    = "systemd-version"
	CheckProbeProperties   = "probe-unit-properties"
	CheckCgroupV2          = "cgroup-v2"
	CheckDelegation        = "user-manager-delegation"
	CheckLinger            = "linger"
	CheckSocketPath        = "runtime-dir-socket-length"
	CheckBootID            = "boot-id"
	CheckKVM               = "dev-kvm"
	CheckManagerKVM        = "user-manager-kvm-group"
	CheckTun               = "dev-net-tun"
	CheckPastaUserns       = "pasta-userns"
	CheckAppArmor          = "apparmor-pasta-profile"
	CheckSubids            = "subuid-subgid-range"
	CheckUIDMapHelpers     = "newuidmap-newgidmap"
	CheckUIDMapNamespace   = "newuidmap-first-vm-range"
	CheckStateACL          = "state-dir-posix-acl"
	CheckOomd              = "systemd-oomd"
	CheckFDStore           = "user-manager-fd-store"
	CheckSlotHostname      = "k3s-slot-hostname"
	CheckTzdata            = "tzdata"
	VMSlotIDs              = 65536
	MaxSocketPath          = 108
	LongestRuntimeSocket   = "/socialweb-vm-0123456789-abcdefghijklmnopqrstuvwxyz012345/fc.sock"
	ProbePrefix            = "socialweb-probe-"
	AppArmorProfileName    = "socialweb-pasta"
	DelegatedControllers   = "cpu memory pids"
	InstallCommandTemplate = "socialweb-computer install %s (as root)"
)

type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type Helper struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Setuid bool   `json:"setuidRoot"`
	Caps   bool   `json:"fileCaps"`
	Err    string `json:"err,omitempty"`
}

type Facts struct {
	Role                  Role
	Account               string
	UID                   int
	Euid                  int
	StateDir              string
	SystemdVersion        string
	VersionErr            string
	ProbeSent             map[string]any
	ProbeReadBack         map[string]any
	ProbeErr              string
	CgroupV2              bool
	ManagerControllers    []string
	ManagerControllersErr string
	Linger                bool
	RuntimeDir            string
	BootID                string
	KVMErr                string
	KVMMissing            bool
	KVMGid                int
	AccountInKVM          bool
	ManagerPID            int
	ManagerGroups         []int
	ManagerErr            string
	TunErr                string
	PastaChecked          bool
	PastaErr              string
	AppArmorRestricts     bool
	AppArmorLoaded        bool
	AppArmorErr           string
	Subuid                string
	Subgid                string
	Helpers               []Helper
	UIDMapChecked         bool
	UIDMapErr             string
	ACLChecked            bool
	ACLErr                string
	OomdActive            bool
	Tzdata                bool
}

func installFix(role Role) string { return fmt.Sprintf(InstallCommandTemplate, role) }

func Evaluate(f Facts) []Result {
	out := []Result{account(f), systemdVersion(f), probeProperties(f), cgroupV2(f), delegation(f), linger(f),
		socketPath(f), bootID(f)}
	if f.Role == RoleBidder {
		out = append(out, kvm(f), managerKVM(f), tun(f), pastaUserns(f), apparmor(f), subids(f), uidmapHelpers(f),
			uidmapNamespace(f), stateACL(f))
	}
	out = append(out, oomd(f))
	if f.Role == RoleGateway {
		out = append(out, fdStore(f), slotHostname(), tzdata(f))
	}
	return out
}

func Failed(rs []Result) []string {
	var out []string
	for _, r := range rs {
		if r.Status == StatusFail {
			out = append(out, r.Name)
		}
	}
	return out
}

func Write(w io.Writer, rs []Result, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(rs, "", "  ")
		fmt.Fprintln(w, string(b))
		return
	}
	for _, r := range rs {
		fmt.Fprintf(w, "%-5s %-28s %s\n", strings.ToUpper(string(r.Status)), r.Name, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(w, "      %-28s fix: %s\n", "", r.Fix)
		}
	}
}

func ok(name, detail string) Result { return Result{Name: name, Status: StatusOK, Detail: detail} }

func fail(name, detail, fix string) Result {
	return Result{Name: name, Status: StatusFail, Detail: detail, Fix: fix}
}

func account(f Facts) Result {
	if f.Euid == 0 {
		return fail(CheckAccount, "running as root; the host contract is the account's view",
			fmt.Sprintf("run check-host as the account (install runs it as the account after %s)", installFix(f.Role)))
	}
	return ok(CheckAccount, fmt.Sprintf("%s (uid %d)", f.Account, f.UID))
}

func systemdVersion(f Facts) Result {
	if f.VersionErr != "" {
		return fail(CheckSystemdVersion, "the user manager's private socket did not answer: "+f.VersionErr,
			"enable linger for the account so user@<uid> runs: "+installFix(f.Role))
	}
	n, err := strs.LeadingInt(f.SystemdVersion)
	if err != nil || n < hostsvc.MinSystemdVersion {
		return fail(CheckSystemdVersion,
			fmt.Sprintf("the user manager reports systemd %q; %d or newer is required (GetUnitByPIDFD)", f.SystemdVersion, hostsvc.MinSystemdVersion),
			fmt.Sprintf("upgrade the host to a distribution shipping systemd >= %d", hostsvc.MinSystemdVersion))
	}
	return ok(CheckSystemdVersion, "systemd "+f.SystemdVersion)
}

func ProbeSpec(role Role, suffix string, exec []string, outputFile string) (hostsvc.UnitSpec, map[string]any) {
	spec := hostsvc.UnitSpec{
		Name:        ProbePrefix + suffix + hostsvc.ServiceSuffix,
		Description: "socialweb-computer check-host property probe",
		Exec:        exec,
		Env:         []string{"SOCIALWEB_PROBE=1"},
		Output:      hostsvc.Output{StdoutFile: outputFile, StderrFile: outputFile},
		Limits:      hostsvc.Limits{MemoryMax: 64 << 20, CPUWeight: 50, CPUQuotaPercent: 50, TasksMax: 16},
		OOMPolicy:   "kill",
		KillSignal:  syscall.SIGTERM,
		StopTimeout: 5 * time.Second,
		After:       []string{"basic.target"},
		Wants:       []string{"basic.target"},
		Slice:       hostsvc.DefaultSlice,
	}
	extra := map[string]any{}
	if role == RoleGateway {
		extra["FileDescriptorStoreMax"] = uint32(1)
	}
	return spec, extra
}

var readBackName = map[string]string{
	"StandardOutputFileToAppend": "StandardOutput",
	"StandardErrorFileToAppend":  "StandardError",
}

func PropertyMismatches(sent, back map[string]any) []string {
	var bad []string
	for name, want := range sent {
		readName := name
		if rn, ok := readBackName[name]; ok {
			readName = rn
		}
		got, present := back[readName]
		if !present || got == nil {
			bad = append(bad, name+" (not read back)")
			continue
		}
		if !propertyMatches(name, want, got) {
			bad = append(bad, fmt.Sprintf("%s (sent %v, read back %v)", name, want, got))
		}
	}
	sort.Strings(bad)
	return bad
}

func propertyMatches(name string, want, got any) bool {
	switch name {
	case "StandardOutputFileToAppend", "StandardErrorFileToAppend":
		return fmt.Sprint(got) == "append"
	case "ExecStart":
		return execPath(want) != "" && strings.Contains(fmt.Sprint(got), execPath(want))
	case "After", "Wants", "Environment":
		return containsAll(got, want)
	}
	return numberOrString(want) == numberOrString(got)
}

func execPath(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice || rv.Len() == 0 {
		return ""
	}
	first := rv.Index(0)
	if first.Kind() == reflect.Struct {
		if p := first.FieldByName("Path"); p.IsValid() {
			return p.String()
		}
	}
	return fmt.Sprint(first.Interface())
}

func stringsOf(v any) []string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil
	}
	out := make([]string, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out = append(out, fmt.Sprint(rv.Index(i).Interface()))
	}
	return out
}

func containsAll(got, want any) bool {
	have := map[string]bool{}
	for _, s := range stringsOf(got) {
		have[s] = true
	}
	for _, s := range stringsOf(want) {
		if !have[s] {
			return false
		}
	}
	return true
}

func numberOrString(v any) string {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	}
	return fmt.Sprint(v)
}

func probeProperties(f Facts) Result {
	if f.ProbeErr != "" {
		return fail(CheckProbeProperties, "the probe unit did not start: "+f.ProbeErr,
			"read the probe's start error above; every runtime property must be accepted by the user manager")
	}
	if len(f.ProbeSent) == 0 {
		return fail(CheckProbeProperties, "no probe unit was started", "re-run check-host as the account")
	}
	if bad := PropertyMismatches(f.ProbeSent, f.ProbeReadBack); len(bad) > 0 {
		return fail(CheckProbeProperties, "the user manager did not keep: "+strings.Join(bad, ", "),
			fmt.Sprintf("use a host whose systemd (>= %d) supports every property the runtime sets", hostsvc.MinSystemdVersion))
	}
	names := make([]string, 0, len(f.ProbeSent))
	for n := range f.ProbeSent {
		names = append(names, n)
	}
	sort.Strings(names)
	return ok(CheckProbeProperties, fmt.Sprintf("%d properties read back: %s", len(names), strings.Join(names, " ")))
}

func cgroupV2(f Facts) Result {
	if !f.CgroupV2 {
		return fail(CheckCgroupV2, "/sys/fs/cgroup is not the cgroup v2 unified hierarchy",
			"boot with systemd.unified_cgroup_hierarchy=1 (the default on current distributions)")
	}
	return ok(CheckCgroupV2, "/sys/fs/cgroup is cgroup2")
}

func delegation(f Facts) Result {
	if f.ManagerControllersErr != "" {
		return fail(CheckDelegation, "reading user@"+strconv.Itoa(f.UID)+".service cgroup.controllers: "+f.ManagerControllersErr,
			"enable linger for the account: "+installFix(f.Role))
	}
	have := map[string]bool{}
	for _, c := range f.ManagerControllers {
		have[c] = true
	}
	var missing []string
	for c := range strings.FieldsSeq(DelegatedControllers) {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fail(CheckDelegation, fmt.Sprintf("user@%d.service is not delegated %s (has %s): CPUQuota would be accepted but unenforced",
			f.UID, strings.Join(missing, " "), strings.Join(f.ManagerControllers, " ")),
			fmt.Sprintf("%s writes /etc/systemd/system/user@%d.service.d/socialweb-delegate.conf (Delegate=%s); then restart user@%d (stops every VM) or reboot",
				installFix(f.Role), f.UID, DelegatedControllers, f.UID))
	}
	return ok(CheckDelegation, "user@"+strconv.Itoa(f.UID)+".service controllers: "+strings.Join(f.ManagerControllers, " "))
}

func linger(f Facts) Result {
	if !f.Linger {
		return fail(CheckLinger, "linger is off for "+f.Account+": the user manager stops at the last logout and every VM with it",
			installFix(f.Role)+" enables linger once the account, its groups and the delegation drop-in are in place")
	}
	return ok(CheckLinger, "linger on for "+f.Account)
}

func socketPath(f Facts) Result {
	if f.RuntimeDir == "" {
		return fail(CheckSocketPath, "XDG_RUNTIME_DIR is unset", "run as the lingering account so /run/user/<uid> exists")
	}
	n := len(f.RuntimeDir) + len(LongestRuntimeSocket)
	if n >= MaxSocketPath {
		return fail(CheckSocketPath, fmt.Sprintf("the longest runtime socket path is %d bytes under %s; the limit is %d", n, f.RuntimeDir, MaxSocketPath-1),
			"use the default /run/user/<uid> runtime directory")
	}
	return ok(CheckSocketPath, fmt.Sprintf("longest runtime socket %d bytes (< %d) under %s", n, MaxSocketPath, f.RuntimeDir))
}

func bootID(f Facts) Result {
	if f.BootID == "" {
		return fail(CheckBootID, "/proc/sys/kernel/random/boot_id is not readable", "mount /proc normally; reboot detection needs boot_id")
	}
	return ok(CheckBootID, f.BootID)
}

func kvm(f Facts) Result {
	switch {
	case f.KVMMissing:
		return fail(CheckKVM, "/dev/kvm does not exist", "enable hardware virtualization and load kvm_intel or kvm_amd (root); never chmod /dev/kvm")
	case f.KVMErr != "":
		return fail(CheckKVM, "/dev/kvm is not read-write for "+f.Account+": "+f.KVMErr,
			installFix(RoleBidder)+" adds the account to the kvm group before linger; never chmod /dev/kvm")
	}
	return ok(CheckKVM, "/dev/kvm read-write for "+f.Account)
}

func managerKVM(f Facts) Result {
	if f.ManagerErr != "" {
		return fail(CheckManagerKVM, "reading user@"+strconv.Itoa(f.UID)+"'s groups: "+f.ManagerErr,
			"enable linger for the account: "+installFix(RoleBidder))
	}
	if slices.Contains(f.ManagerGroups, f.KVMGid) {
		return ok(CheckManagerKVM, fmt.Sprintf("user@%d (pid %d) runs with kvm (gid %d)", f.UID, f.ManagerPID, f.KVMGid))
	}
	if f.AccountInKVM {
		return fail(CheckManagerKVM,
			fmt.Sprintf("the account is in kvm in /etc/group but its user manager (user@%d, pid %d) started before", f.UID, f.ManagerPID),
			fmt.Sprintf("restart user@%d - this stops every VM - or reboot; logging in again does not refresh a lingering manager", f.UID))
	}
	return fail(CheckManagerKVM, fmt.Sprintf("%s is not in the kvm group and user@%d (pid %d) runs without it", f.Account, f.UID, f.ManagerPID),
		installFix(RoleBidder)+" adds kvm before the user manager first starts; on an already running manager it refuses and names the restart")
}

func tun(f Facts) Result {
	if f.TunErr != "" {
		return fail(CheckTun, "/dev/net/tun is not usable: "+f.TunErr, "load the tun module (root): modprobe tun")
	}
	return ok(CheckTun, "/dev/net/tun opens read-write")
}

func pastaUserns(f Facts) Result {
	if !f.PastaChecked {
		return fail(CheckPastaUserns, "the embedded pasta was not resolved: "+f.PastaErr,
			"give check-host the pasta path (-pasta) in a dev build; a release build extracts it")
	}
	if f.PastaErr != "" {
		fix := "check kernel.unprivileged_userns_clone and user.max_user_namespaces"
		if f.AppArmorRestricts {
			fix = "sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 (root; persist it in /etc/sysctl.d), or " + installFix(RoleBidder) +
				" loads /etc/apparmor.d/" + AppArmorProfileName + " for <state>/runtime/*/pasta"
		}
		return fail(CheckPastaUserns, "the embedded pasta did not create a user+net namespace and exec through /bin/sh: "+f.PastaErr, fix)
	}
	return ok(CheckPastaUserns, "the embedded pasta created a user+net namespace and exec'd through /bin/sh")
}

func apparmor(f Facts) Result {
	if !f.AppArmorRestricts {
		return ok(CheckAppArmor, "AppArmor does not restrict unprivileged user namespaces here; no profile needed")
	}
	if f.AppArmorErr != "" {
		return fail(CheckAppArmor, "reading the loaded AppArmor profiles: "+f.AppArmorErr, installFix(RoleBidder))
	}
	if !f.AppArmorLoaded {
		return fail(CheckAppArmor, "kernel.apparmor_restrict_unprivileged_userns=1 and profile "+AppArmorProfileName+" is not loaded",
			installFix(RoleBidder)+" writes /etc/apparmor.d/"+AppArmorProfileName+" and loads it with apparmor_parser -r")
	}
	return ok(CheckAppArmor, "profile "+AppArmorProfileName+" loaded")
}

type SubidEntry struct {
	Owner string
	Start uint64
	Count uint64
	Line  int
}

func (e SubidEntry) End() uint64 { return e.Start + e.Count }

func ParseSubids(content string) []SubidEntry {
	var out []SubidEntry
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 3 {
			continue
		}
		start, err1 := strconv.ParseUint(f[1], 10, 64)
		count, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, SubidEntry{Owner: f[0], Start: start, Count: count, Line: i})
	}
	return out
}

func owns(e SubidEntry, account string, uid int) bool {
	return e.Owner == account || e.Owner == strconv.Itoa(uid)
}

func AccountRange(content, account string, uid int) (SubidEntry, bool) {
	for _, e := range ParseSubids(content) {
		if owns(e, account, uid) {
			return e, true
		}
	}
	return SubidEntry{}, false
}

func Overlaps(a, b SubidEntry) bool { return a.Start < b.End() && b.Start < a.End() }

func SubidProblems(subuid, subgid, account string, uid int) []string {
	var problems []string
	ranges := map[string]SubidEntry{}
	for _, file := range []struct{ name, content string }{{"/etc/subuid", subuid}, {"/etc/subgid", subgid}} {
		var mine []SubidEntry
		for _, e := range ParseSubids(file.content) {
			if owns(e, account, uid) {
				mine = append(mine, e)
			}
		}
		switch {
		case len(mine) == 0:
			problems = append(problems, "no range for "+account+" in "+file.name)
			continue
		case len(mine) > 1:
			problems = append(problems, fmt.Sprintf("%d ranges for %s in %s; install reserves exactly one", len(mine), account, file.name))
		}
		if mine[0].Count < VMSlotIDs {
			problems = append(problems, fmt.Sprintf("%s range of %s holds %d ids, under one VM slot (%d)", file.name, account, mine[0].Count, VMSlotIDs))
		}
		ranges[file.name] = mine[0]
	}
	for fname, r := range ranges {
		for _, other := range []struct{ name, content string }{{"/etc/subuid", subuid}, {"/etc/subgid", subgid}} {
			for _, e := range ParseSubids(other.content) {
				if owns(e, account, uid) {
					continue
				}
				if Overlaps(r, e) {
					problems = append(problems, fmt.Sprintf("%s range %d-%d of %s overlaps %s's %d-%d in %s",
						fname, r.Start, r.End()-1, account, e.Owner, e.Start, e.End()-1, other.name))
				}
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func subids(f Facts) Result {
	if p := SubidProblems(f.Subuid, f.Subgid, f.Account, f.UID); len(p) > 0 {
		return fail(CheckSubids, strings.Join(p, "; "),
			installFix(RoleBidder)+" reserves one contiguous range in both files above every existing entry; or keep exactly one range by hand: usermod --del-subuids/--del-subgids the extra ones, then usermod --add-subuids/--add-subgids <first>-<last> (65536 ids per VM, 16 VMs need 1048576)")
	}
	r, _ := AccountRange(f.Subuid, f.Account, f.UID)
	return ok(CheckSubids, fmt.Sprintf("%d-%d (%d VM slots) in /etc/subuid and /etc/subgid, overlapping nothing", r.Start, r.End()-1, r.Count/VMSlotIDs))
}

func uidmapHelpers(f Facts) Result {
	var bad, good []string
	for _, h := range f.Helpers {
		switch {
		case h.Err != "":
			bad = append(bad, h.Name+": "+h.Err)
		case !h.Setuid && !h.Caps:
			bad = append(bad, h.Path+" is neither setuid root nor carries file capabilities")
		default:
			good = append(good, h.Path)
		}
	}
	if len(f.Helpers) == 0 {
		bad = append(bad, "newuidmap and newgidmap were not looked up")
	}
	if len(bad) > 0 {
		return fail(CheckUIDMapHelpers, strings.Join(bad, "; "),
			"install the distribution's uidmap (Debian/Ubuntu) or shadow-utils (Fedora/RHEL) package (root); never ship a setuid helper")
	}
	return ok(CheckUIDMapHelpers, strings.Join(good, " "))
}

func uidmapNamespace(f Facts) Result {
	if !f.UIDMapChecked {
		return fail(CheckUIDMapNamespace, "not attempted: "+f.UIDMapErr, "fix "+CheckSubids+" and "+CheckUIDMapHelpers+" first")
	}
	if f.UIDMapErr != "" {
		return fail(CheckUIDMapNamespace, "a throwaway user namespace could not be mapped into the first VM sub-range: "+f.UIDMapErr,
			"check the account's /etc/subuid and /etc/subgid range and that newuidmap/newgidmap are the distribution's")
	}
	return ok(CheckUIDMapNamespace, "a throwaway user namespace mapped 0-65535 onto the first VM sub-range")
}

func stateACL(f Facts) Result {
	if !f.ACLChecked || f.ACLErr != "" {
		return fail(CheckStateACL, "POSIX ACLs on "+f.StateDir+": "+f.ACLErr,
			"put the state dir on a filesystem mounted with ACL support (ext4, xfs, btrfs default)")
	}
	return ok(CheckStateACL, "POSIX ACLs supported on "+f.StateDir)
}

func oomd(f Facts) Result {
	if f.OomdActive {
		return Result{Name: CheckOomd, Status: StatusInfo,
			Detail: "systemd-oomd is active; every unit runs below socialweb.slice, outside the app.slice it watches"}
	}
	return Result{Name: CheckOomd, Status: StatusInfo, Detail: "systemd-oomd is not active"}
}

func fdStore(f Facts) Result {
	want, sent := f.ProbeSent["FileDescriptorStoreMax"]
	if !sent {
		return fail(CheckFDStore, "the probe did not ask for an fd store", "re-run check-host --role gateway")
	}
	if got, ok := f.ProbeReadBack["FileDescriptorStoreMax"]; !ok || numberOrString(got) != numberOrString(want) {
		return fail(CheckFDStore, "the user manager did not keep FileDescriptorStoreMax on a transient service",
			"use a systemd whose user manager keeps an fd store for transient services (the kcp front restarts without a refused-connection window only with it)")
	}
	return ok(CheckFDStore, "FileDescriptorStoreMax read back from a transient service")
}

func slotHostname() Result {
	return Result{Name: CheckSlotHostname, Status: StatusInfo,
		Detail: "the k3s slot hostname mechanism is chosen by 0006 M3-CP-SLOTS; probed once it lands"}
}

func tzdata(f Facts) Result {
	if !f.Tzdata {
		return fail(CheckTzdata, "/usr/share/zoneinfo/UTC is missing: the embedded postgres is built with the system tzdata path",
			"install the distribution's tzdata package (root)")
	}
	return ok(CheckTzdata, "/usr/share/zoneinfo present")
}

package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimagerelease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/progress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

//go:embed templates
var templates embed.FS

const (
	AccountGateway       = "socialweb"
	AccountBidder        = "swc-bidder"
	BidderProfile        = "prod-qr"
	StateRoot            = "/var/lib/socialweb-computer"
	DefaultVMSlots       = 256
	SubidFloor           = 100000
	KVMGroup             = "kvm"
	CoreName             = "socialweb-computer"
	GatewayUnit          = hostsvc.NamePrefix + table.GatewayCore
	BidderTemplate       = hostsvc.NamePrefix + table.BidderCore + hostsvc.ServiceSuffix
	SliceUnit            = "socialweb.slice"
	DropInName           = "socialweb-delegate.conf"
	AppArmorPath         = "/etc/apparmor.d/" + hostcontract.AppArmorProfileName
	NodePassword         = "/etc/rancher/node/password"
	LingerDir            = "/var/lib/systemd/linger"
	RestrictUserns       = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	WaitReport           = 10 * time.Second
	DefaultReadyBudget   = bootstrap.DefaultReadyTimeout
	DefaultNetworkBudget = 3 * time.Minute
	MaxReadyBudget       = 10 * time.Minute
	MaxNetworkBudget     = 30 * time.Minute
	HostCheckBudget      = 2 * time.Minute
	CommandBudget        = time.Minute
	MinImageBudget       = 30 * time.Second
	StartEndedGrace      = 6 * time.Second
	BidderStartGrace     = 6 * time.Minute
	StartJobGrace        = time.Minute
	AppArmorLoaded       = "/sys/kernel/security/apparmor/profiles"
	UserUnitDir          = ".config/systemd/user"
	WantsDir             = "default.target.wants"
	RuntimePrefix        = hostsvc.NamePrefix
	RuntimeSliceTop      = "socialweb"
)

var (
	ErrProcBackend       = errors.New("install: install places production units on the systemd-user backend only; host-backend proc is the dev run mode (socialweb-computer dev up)")
	ErrForeignAccount    = errors.New("install: refusing to take over an existing account no install record says install created")
	ErrManagerWithoutKVM = errors.New("install: the account's user manager already runs without the kvm group")
	ErrVMsRunning        = errors.New("uninstall: VM units of the account are active; stopping one makes its VM Lost")
	ErrNoRecord          = errors.New("uninstall: no install record for this account")
	ErrUsage             = errors.New("install: refused the arguments")
	ErrBinaryMismatch    = errors.New("install: bin/current is not the binary this install was invoked as")
	ErrUnsupported       = errors.New("install: install and uninstall are Linux root steps; this OS has no backend (plans 0011, 0012)")
	handleRE             = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	accountRE            = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
	stateRE              = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

type Account struct {
	Name string
	UID  int
	GID  int
	Home string
}

type UserManager interface {
	Reload(ctx context.Context) error
	EnableNow(ctx context.Context, units []string) error
	Restart(ctx context.Context, units []string) error
	DisableStop(ctx context.Context, units []string) error
	List(ctx context.Context) ([]hostsvc.UnitStatus, error)
	Stop(ctx context.Context, unit string) error
	Close()
}

type System interface {
	LookupAccount(name string) (Account, bool, error)
	CreateAccount(name, home string) error
	DeleteAccount(name string) error
	AddToGroup(account, group string) error
	RemoveFromGroup(account, group string) error
	Chown(path string, uid, gid int) error
	Owner(path string) (int, int, error)
	ReloadSystemManager(ctx context.Context) error
	LoadAppArmor(path string) error
	UnloadAppArmor(path string) error
	SetLinger(ctx context.Context, a Account, on bool) error
	User(ctx context.Context, a Account) (UserManager, error)
	CheckHost(ctx context.Context, a Account, binary string, role hostcontract.Role, stateDir string) error
}

type Options struct {
	Role                   hostcontract.Role
	Handles                []string
	Account                string
	StateDir               string
	Binary                 string
	VMSlots                int
	DryRun                 bool
	AdoptAccount           bool
	HostBackend            string
	Root                   string
	Staged                 bool
	UnitPrefix             string
	ReadyTimeout           string
	NetworkTimeout         string
	GuestImageBase         string
	GuestImageBaseResolved bool
	Settings               map[string]string
	Out                    io.Writer
}

const (
	OptKcpFront       = "kcp-front-address"
	OptTLSFront       = "tls-front-address"
	OptCertFront      = "cert-front-address"
	OptSpecFile       = "gateway-spec-file"
	OptGuestImg       = "guest-image-source"
	OptGuestImgBase   = "guest-image-release-base"
	OptReadyTimeout   = "ready-timeout"
	OptNetworkTimeout = "network-timeout"
)

var GatewayListen = map[string]string{OptKcpFront: "127.0.0.1:6443", OptTLSFront: "127.0.0.1:8443", OptCertFront: "127.0.0.1:8444"}

var Passed = []string{OptKcpFront, OptTLSFront, OptCertFront, OptSpecFile, OptGuestImg, OptGuestImgBase}

func Rooted(root string) bool { return root != "" && filepath.Clean(root) != "/" }

func prefixOr(p string) string {
	if p == "" {
		return hostsvc.NamePrefix
	}
	return p
}

type StepResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

type Report struct {
	Steps   []StepResult `json:"steps"`
	Changes []string     `json:"changes"`
}

func DefaultAccount(role hostcontract.Role) string {
	if role == hostcontract.RoleBidder {
		return AccountBidder
	}
	return AccountGateway
}

func RecordPath(account string) string { return filepath.Join(StateRoot, "install-"+account+".json") }

func HandleID(handle string) string {
	s := sha256.Sum256([]byte(handle))
	return hex.EncodeToString(s[:])[:10]
}

func BidderLayout(state, prefix, handle string) devinstance.Layout {
	hid := HandleID(handle)
	dir := filepath.Join(state, "handles", hid)
	kcp := filepath.Join(dir, "kcp")
	run := filepath.Join(kcp, "run")
	return devinstance.Layout{ID: "bidder-" + hid, Short: hid, StateDir: dir, RuntimeDir: run,
		Socket: filepath.Join(run, "sup.sock"), SupState: filepath.Join(dir, "sup"),
		Record: filepath.Join(dir, "bidder.json"), CoreRecord: filepath.Join(dir, "core.json"),
		Lock: filepath.Join(kcp, "instance.lock"), CoreLock: filepath.Join(kcp, "core.lock"), Logs: filepath.Join(dir, "logs"),
		KineDir: filepath.Join(kcp, "kine-a"), KcpRoot: filepath.Join(kcp, "slot-a"), Prefix: prefix}
}

func CoreUnits(role hostcontract.Role, prefix string, handles []string) []string {
	if role == hostcontract.RoleGateway {
		return []string{table.CoreTemplate(prefix, table.RoleGateway)}
	}
	out := make([]string, 0, len(handles))
	for _, h := range handles {
		out = append(out, prefixOr(prefix)+table.BidderCore+h+hostsvc.ServiceSuffix)
	}
	sort.Strings(out)
	return out
}

type installer struct {
	ctx      context.Context
	o        Options
	sys      System
	rep      *Report
	rec      *Record
	recPath  string
	acct     Account
	known    bool
	cfgWrote bool
	wrote    map[string]bool
	planned  map[string]bool
	mu       sync.Mutex
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

func (in *installer) path(p string) string {
	if in.o.Staged && in.o.StateDir != "" && (within(p, in.o.StateDir) || within(in.o.StateDir, p)) {
		return p
	}
	return filepath.Join(in.o.Root, p)
}

func (in *installer) out() io.Writer {
	if in.o.Out == nil {
		return io.Discard
	}
	return in.o.Out
}

func (in *installer) printf(format string, a ...any) {
	in.mu.Lock()
	defer in.mu.Unlock()
	fmt.Fprintf(in.out(), format, a...)
}

func (in *installer) change(desc string, f func() error) error {
	in.rep.Changes = append(in.rep.Changes, desc)
	if in.o.DryRun {
		in.printf("would %s\n", desc)
		return nil
	}
	in.printf("%s\n", desc)
	if err := f(); err != nil {
		return fmt.Errorf("install: %s: %w", desc, err)
	}
	return nil
}

func (in *installer) wait(desc string, f func() error) error {
	if in.o.DryRun {
		in.printf("would %s\n", desc)
		return nil
	}
	started := time.Now()
	in.printf("%s\n", desc)
	stop := make(chan struct{})
	var ticking sync.WaitGroup
	ticking.Go(func() {
		t := time.NewTicker(WaitReport)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				in.printf("  ... %s\n", progress.Elapsed(time.Since(started)))
			}
		}
	})
	err := f()
	close(stop)
	ticking.Wait()
	if err != nil {
		in.printf("  failed after %s\n", progress.Elapsed(time.Since(started)))
		return fmt.Errorf("install: %s: %w", desc, err)
	}
	in.printf("  done after %s\n", progress.Elapsed(time.Since(started)))
	return nil
}

func (in *installer) changeWait(desc string, f func() error) error {
	in.rep.Changes = append(in.rep.Changes, desc)
	return in.wait(desc, f)
}

func (in *installer) step(name, outcome, detail string) {
	in.rep.Steps = append(in.rep.Steps, StepResult{Name: name, Outcome: outcome, Detail: detail})
	if outcome == OutcomeNotNeeded {
		return
	}
	in.rec.set(name, outcome)
}

func (in *installer) saveRecord() error {
	if in.o.DryRun {
		return nil
	}
	b := in.rec.bytes()
	if sameBytes(in.recPath, b) {
		return in.ensureOwner(in.recPath, recordMode, 0, 0)
	}
	return in.change("write the install record "+RecordPath(in.rec.Account), func() error {
		if err := os.MkdirAll(filepath.Dir(in.recPath), 0o755); err != nil {
			return err
		}
		return writeAtomic(in.recPath, b, recordMode)
	})
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".swc-install-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (in *installer) ensureOwner(full string, mode os.FileMode, uid, gid int) error {
	st, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != mode.Perm() && st.Mode()&fs.ModeSymlink == 0 {
		if err := in.change(fmt.Sprintf("chmod %o %s", mode.Perm(), strings.TrimPrefix(full, in.o.Root)), func() error { return os.Chmod(full, mode) }); err != nil {
			return err
		}
	}
	if uid < 0 {
		return nil
	}
	ou, og, err := in.sys.Owner(full)
	if err != nil {
		return err
	}
	if ou == uid && og == gid {
		return nil
	}
	return in.change(fmt.Sprintf("chown %d:%d %s", uid, gid, strings.TrimPrefix(full, in.o.Root)), func() error { return in.sys.Chown(full, uid, gid) })
}

func (in *installer) ensureDir(host string, mode os.FileMode, uid, gid int, track bool) error {
	full := in.path(host)
	if in.planned[host] {
		return nil
	}
	if _, err := os.Stat(full); errors.Is(err, fs.ErrNotExist) {
		parent := filepath.Dir(host)
		if parent != host {
			if _, err := os.Stat(in.path(parent)); errors.Is(err, fs.ErrNotExist) {
				if err := in.ensureDir(parent, 0o755, 0, 0, track); err != nil {
					return err
				}
			}
		}
		if err := in.change("create directory "+host, func() error { return os.Mkdir(full, mode) }); err != nil {
			return err
		}
		if track {
			in.rec.addDir(host)
		}
		if in.o.DryRun {
			if in.planned == nil {
				in.planned = map[string]bool{}
			}
			in.planned[host] = true
			return nil
		}
	}
	return in.ensureOwner(full, mode, uid, gid)
}

func (in *installer) ensureFile(host string, content []byte, mode os.FileMode, uid, gid int) (bool, error) {
	full := in.path(host)
	if sameBytes(full, content) {
		return false, in.ensureOwner(full, mode, uid, gid)
	}
	err := in.change("write "+host, func() error {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return writeAtomic(full, content, mode)
	})
	if err != nil || in.o.DryRun {
		return true, err
	}
	return true, in.ensureOwner(full, mode, uid, gid)
}

func (in *installer) read(host string) string {
	b, _ := os.ReadFile(in.path(host))
	return string(b)
}

func validate(o *Options) error {
	if o.HostBackend != "" && o.HostBackend != string(hostsvc.BackendSystemdUser) {
		if o.HostBackend == string(hostsvc.BackendProc) {
			return ErrProcBackend
		}
		return fmt.Errorf("install: host-backend %q: install writes %s", o.HostBackend, hostsvc.BackendSystemdUser)
	}
	if _, err := hostcontract.ParseRole(string(o.Role)); err != nil {
		return err
	}
	if o.Account == "" {
		o.Account = DefaultAccount(o.Role)
	}
	if !accountRE.MatchString(o.Account) {
		return fmt.Errorf("install: account name %q is not a valid system account name", o.Account)
	}
	if o.StateDir == "" {
		o.StateDir = filepath.Join(StateRoot, o.Account)
	}
	if !stateRE.MatchString(o.StateDir) || filepath.Clean(o.StateDir) != o.StateDir {
		return fmt.Errorf("install: state dir %q must be a clean absolute path of [A-Za-z0-9._/-] (it is written into unit files)", o.StateDir)
	}
	if o.VMSlots == 0 {
		o.VMSlots = DefaultVMSlots
	}
	if o.VMSlots < 1 {
		return fmt.Errorf("install: --vm-slots %d: want at least 1", o.VMSlots)
	}
	if o.Root == "" {
		o.Root = "/"
	}
	if o.UnitPrefix != "" {
		if err := hostsvc.ValidatePrefix(o.UnitPrefix); err != nil {
			return fmt.Errorf("install: unit-prefix: %w", err)
		}
		if !strings.HasSuffix(o.UnitPrefix, "-") {
			return fmt.Errorf("install: unit-prefix %q must end in -", o.UnitPrefix)
		}
	}
	switch o.Role {
	case hostcontract.RoleGateway:
		if len(o.Handles) > 0 {
			return fmt.Errorf("install gateway takes no --handle")
		}
	case hostcontract.RoleBidder:
		if len(o.Handles) == 0 {
			return fmt.Errorf("install bidder needs at least one --handle (the atproto handle each bidder instance runs as)")
		}
		for _, h := range o.Handles {
			if !handleRE.MatchString(h) {
				return fmt.Errorf("install: handle %q is not a lowercase atproto handle", h)
			}
		}
	}
	if o.Binary == "" || !filepath.IsAbs(o.Binary) {
		return fmt.Errorf("install: the binary to install must be an absolute path, got %q", o.Binary)
	}
	return nil
}

func Install(ctx context.Context, o Options, sys System) (Report, error) {
	rep := Report{}
	if err := validate(&o); err != nil {
		return rep, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	in := &installer{ctx: ctx, o: o, sys: sys, rep: &rep, recPath: filepath.Join(o.Root, RecordPath(o.Account))}
	rec, _, err := loadOrNewRecord(in.recPath, o.Role, o.Account, o.StateDir)
	if err != nil {
		return rep, err
	}
	if rec.Role != o.Role {
		return rep, fmt.Errorf("install: %s was installed as a %s; uninstall it first", o.Account, rec.Role)
	}
	if rec.StateDir != o.StateDir {
		return rep, fmt.Errorf("install: %s was installed with state dir %s, not %s", o.Account, rec.StateDir, o.StateDir)
	}
	if len(rec.Steps) > 0 && rec.UnitPrefix != o.UnitPrefix {
		return rep, fmt.Errorf("install: %s was installed with unit prefix %q, not %q", o.Account, prefixOr(rec.UnitPrefix), prefixOr(o.UnitPrefix))
	}
	rec.UnitPrefix = o.UnitPrefix
	in.rec = rec
	if err := in.checkBudgets(); err != nil {
		return rep, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	steps := []func() error{in.account, in.subids, in.kvm, in.delegation, in.apparmor, in.nodePassword, in.stateLayout,
		in.guestImage, in.unitFiles, in.linger, in.enable, in.bringUp, in.checkHost}
	for _, s := range steps {
		if err := s(); err != nil {
			return rep, err
		}
		if err := in.saveRecord(); err != nil {
			return rep, err
		}
	}
	if o.DryRun {
		in.printf("dry run: %d change(s) computed, nothing written\n", len(rep.Changes))
	}
	return rep, nil
}

func (in *installer) account() error {
	name := in.o.Account
	a, ok, err := in.sys.LookupAccount(name)
	if err != nil {
		return err
	}
	if ok {
		switch {
		case in.rec.Steps[StepAccount] == OutcomeCreated || in.rec.Steps[StepAccount] == OutcomeAdopted:
			in.step(StepAccount, OutcomeFound, fmt.Sprintf("%s uid %d (recorded %s)", name, a.UID, in.rec.Steps[StepAccount]))
		case !in.o.AdoptAccount:
			return fmt.Errorf("%w: %s (uid %d) exists and %s does not record it; pass --adopt-account only if this account is meant for socialweb-computer",
				ErrForeignAccount, name, a.UID, RecordPath(name))
		default:
			in.step(StepAccount, OutcomeAdopted, fmt.Sprintf("%s uid %d adopted (--adopt-account)", name, a.UID))
		}
		in.acct, in.known = a, true
		in.rec.UID = a.UID
		return nil
	}
	err = in.change(fmt.Sprintf("create system account %s (home %s, no password, no login shell)", name, in.o.StateDir), func() error {
		return in.sys.CreateAccount(name, in.o.StateDir)
	})
	if err != nil {
		return err
	}
	in.step(StepAccount, OutcomeCreated, name)
	if in.o.DryRun {
		in.acct = Account{Name: name, UID: -1, GID: -1, Home: in.o.StateDir}
		return nil
	}
	if a, ok, err = in.sys.LookupAccount(name); err != nil || !ok {
		return fmt.Errorf("install: %s was created but does not resolve: %v", name, err)
	}
	in.acct, in.known = a, true
	in.rec.UID = a.UID
	if err := in.saveRecord(); err != nil {
		return err
	}
	return nil
}

func (in *installer) uidLabel() string {
	if !in.known {
		return "<uid>"
	}
	return strconv.Itoa(in.acct.UID)
}

func maxEnd(contents ...string) uint64 {
	var end uint64
	for _, c := range contents {
		for _, e := range hostcontract.ParseSubids(c) {
			if e.End() > end {
				end = e.End()
			}
		}
	}
	return end
}

func NextRangeStart(subuid, subgid string) uint64 {
	start := max(maxEnd(subuid, subgid), SubidFloor)
	return (start + hostcontract.VMSlotIDs - 1) / hostcontract.VMSlotIDs * hostcontract.VMSlotIDs
}

func (in *installer) subids() error {
	if in.o.Role != hostcontract.RoleBidder {
		return nil
	}
	files := []struct{ step, host string }{{StepSubuid, "/etc/subuid"}, {StepSubgid, "/etc/subgid"}}
	sub, sg := in.read("/etc/subuid"), in.read("/etc/subgid")
	start := NextRangeStart(sub, sg)
	count := uint64(in.o.VMSlots) * hostcontract.VMSlotIDs
	reusedFrom := ""
	for i, content := range []string{sub, sg} {
		if r, ok := hostcontract.AccountRange(content, in.o.Account, in.acct.UID); ok {
			start, count = r.Start, r.End()-r.Start
			reusedFrom = files[i].host
			break
		}
	}
	for _, f := range files {
		content := in.read(f.host)
		if r, ok := hostcontract.AccountRange(content, in.o.Account, in.acct.UID); ok {
			in.step(f.step, OutcomeFound, fmt.Sprintf("%d-%d", r.Start, r.End()-1))
			continue
		}
		if reusedFrom != "" {
			want := hostcontract.SubidEntry{Start: start, Count: count}
			for _, e := range hostcontract.ParseSubids(content) {
				if hostcontract.Overlaps(want, e) {
					detail := fmt.Sprintf("the range %d-%d of %s in %s overlaps %s:%d:%d on line %d of %s",
						start, start+count-1, in.o.Account, reusedFrom, e.Owner, e.Start, e.Count, e.Line+1, f.host)
					in.step(f.step, OutcomeRefused, detail)
					return fmt.Errorf("install: %s; give %s one range that is free in both /etc/subuid and /etc/subgid", detail, in.o.Account)
				}
			}
		}
		line := fmt.Sprintf("%s:%d:%d\n", in.o.Account, start, count)
		next := content
		if next != "" && !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		next += line
		full := in.path(f.host)
		mode := os.FileMode(0o644)
		if st, err := os.Stat(full); err == nil {
			mode = st.Mode().Perm()
		}
		if in.rec.SubidLines == nil {
			in.rec.SubidLines = map[string]string{}
		}
		in.rec.SubidLines[f.host] = line
		in.rec.set(f.step, OutcomeCreated)
		if err := in.saveRecord(); err != nil {
			return err
		}
		err := in.change(fmt.Sprintf("reserve %s in %s (%d VM slots of %d ids)", strings.TrimSpace(line), f.host, in.o.VMSlots, hostcontract.VMSlotIDs), func() error {
			return writeAtomic(full, []byte(next), mode)
		})
		if err != nil {
			return err
		}
		in.step(f.step, OutcomeCreated, strings.TrimSpace(line))
	}
	return nil
}

func (in *installer) kvm() error {
	if in.o.Role != hostcontract.RoleBidder {
		return nil
	}
	gid, member := hostcontract.GroupMembership(in.read("/etc/group"), KVMGroup, in.o.Account)
	if gid < 0 {
		return fmt.Errorf("install: no %s group in /etc/group: load kvm_intel or kvm_amd and install the distribution's kvm/udev rules (never chmod /dev/kvm)", KVMGroup)
	}
	if member {
		in.step(StepKVM, OutcomeFound, fmt.Sprintf("%s in %s (gid %d)", in.o.Account, KVMGroup, gid))
	} else {
		if err := in.change("add "+in.o.Account+" to group "+KVMGroup+" (before the user manager first starts)", func() error {
			return in.sys.AddToGroup(in.o.Account, KVMGroup)
		}); err != nil {
			return err
		}
		in.step(StepKVM, OutcomeCreated, fmt.Sprintf("gid %d", gid))
	}
	if !in.known {
		return nil
	}
	if in.o.Staged {
		return nil
	}
	pid, groups, notRunning := hostcontract.ManagerGroups(in.o.Root, in.acct.UID)
	if notRunning != "" {
		return nil
	}
	if slices.Contains(groups, gid) {
		return nil
	}
	if err := in.saveRecord(); err != nil {
		return err
	}
	return fmt.Errorf("%w: user@%d (pid %d) started before %s joined %s; restart user@%d - this stops every VM - or reboot, then re-run install (install never restarts it)",
		ErrManagerWithoutKVM, in.acct.UID, pid, in.o.Account, KVMGroup, in.acct.UID)
}

const statePlaceholder = "{{.State}}"

const startTimeoutPlaceholder = "{{.StartTimeout}}"

func render(name string, data unitData) ([]byte, error) {
	raw, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, err
	}
	out := strings.ReplaceAll(string(raw), statePlaceholder, data.State)
	out = strings.ReplaceAll(out, startTimeoutPlaceholder, data.StartTimeout)
	if strings.Contains(out, "{{") {
		return nil, fmt.Errorf("install: template %s has a placeholder other than %s or %s", name, statePlaceholder, startTimeoutPlaceholder)
	}
	return []byte(out), nil
}

type unitData struct {
	State        string
	StartTimeout string
}

func StartTimeoutFor(readyBudget time.Duration) time.Duration {
	return readyBudget + BidderStartGrace
}

func RenderUnits(role hostcontract.Role, state, prefix string, startTimeout time.Duration) (map[string][]byte, error) {
	out := map[string][]byte{}
	core, tmpl := table.CoreTemplate(prefix, table.RoleGateway), GatewayUnit
	if role == hostcontract.RoleBidder {
		core, tmpl = table.CoreTemplate(prefix, table.RoleBidder), BidderTemplate
	}
	data := unitData{State: state, StartTimeout: strconv.Itoa(int(startTimeout.Seconds())) + "s"}
	for name, src := range map[string]string{SliceUnit: SliceUnit, core: tmpl} {
		b, err := render(src, data)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}

func DropIn() ([]byte, error) { return render(DropInName, unitData{}) }

func AppArmorProfile(state string) ([]byte, error) {
	return render(hostcontract.AppArmorProfileName, unitData{State: state})
}

func (in *installer) delegation() error {
	dir := "/etc/systemd/system/user@" + in.uidLabel() + ".service.d"
	content, err := DropIn()
	if err != nil {
		return err
	}
	if err := in.ensureDir(dir, 0o755, 0, 0, true); err != nil {
		return err
	}
	changed, err := in.ensureFile(filepath.Join(dir, DropInName), content, 0o644, 0, 0)
	if err != nil {
		return err
	}
	if !changed {
		in.step(StepDelegation, OutcomeFound, dir)
		return nil
	}
	in.step(StepDelegation, OutcomeCreated, dir+"/"+DropInName+": Delegate="+hostcontract.DelegatedControllers)
	return in.change("systemctl daemon-reload (system manager, for the delegation drop-in)", func() error {
		return in.sys.ReloadSystemManager(in.ctx)
	})
}

func (in *installer) apparmor() error {
	if in.o.Role != hostcontract.RoleBidder {
		return nil
	}
	if strings.TrimSpace(in.read(RestrictUserns)) != "1" {
		in.step(StepAppArmor, OutcomeNotNeeded, "AppArmor does not restrict unprivileged user namespaces")
		return nil
	}
	content, err := AppArmorProfile(in.o.StateDir)
	if err != nil {
		return err
	}
	changed, err := in.ensureFile(AppArmorPath, content, 0o644, 0, 0)
	if err != nil {
		return err
	}
	if changed {
		in.step(StepAppArmor, OutcomeCreated, AppArmorPath)
	} else {
		in.step(StepAppArmor, OutcomeFound, AppArmorPath)
	}
	if !changed && hostcontract.AppArmorProfileLoaded(in.read(AppArmorLoaded), hostcontract.AppArmorProfileName) {
		return nil
	}
	return in.change("apparmor_parser -r "+AppArmorPath, func() error { return in.sys.LoadAppArmor(in.path(AppArmorPath)) })
}

func (in *installer) nodePassword() error {
	if in.o.Role != hostcontract.RoleGateway {
		return nil
	}
	full := in.path(NodePassword)
	if _, err := os.Stat(full); err == nil {
		in.step(StepPassword, OutcomeFound, NodePassword)
		if in.o.DryRun || !in.known {
			return nil
		}
		ou, og, err := in.sys.Owner(full)
		if err != nil {
			return err
		}
		if ou != in.acct.UID {
			if in.rec.PrevOwner == nil {
				in.rec.PrevOwner = map[string][2]int{}
			}
			if _, kept := in.rec.PrevOwner[NodePassword]; !kept {
				in.rec.PrevOwner[NodePassword] = [2]int{ou, og}
			}
			in.rec.set(StepPassword, OutcomeUpdated)
		}
		return in.ensureOwner(full, 0o600, in.acct.UID, in.acct.GID)
	}
	if err := in.ensureDir(filepath.Dir(NodePassword), 0o755, 0, 0, true); err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	if _, err := in.ensureFile(NodePassword, []byte(hex.EncodeToString(secret)), 0o600, in.acct.UID, in.acct.GID); err != nil {
		return err
	}
	in.step(StepPassword, OutcomeCreated, NodePassword+" (0600, the k3s node password)")
	return nil
}

func (in *installer) stateLayout() error {
	uid, gid := in.acct.UID, in.acct.GID
	state := in.o.StateDir
	if err := in.ensureDir(filepath.Dir(state), 0o755, 0, 0, true); err != nil {
		return err
	}
	_, statErr := os.Stat(in.path(state))
	if err := in.ensureDir(state, 0o700, uid, gid, true); err != nil {
		return err
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		in.step(StepStateDir, OutcomeCreated, state)
	} else {
		in.step(StepStateDir, OutcomeFound, state)
	}
	for _, d := range []string{"bin", "runtime"} {
		if err := in.ensureDir(filepath.Join(state, d), 0o755, uid, gid, false); err != nil {
			return err
		}
	}
	if err := in.binary(); err != nil {
		return err
	}
	return in.config()
}

func (in *installer) binary() error {
	sum, err := sha256sum.File(in.o.Binary)
	if err != nil {
		return fmt.Errorf("install: hashing %s: %w", in.o.Binary, err)
	}
	bin := filepath.Join(in.o.StateDir, "bin")
	current := filepath.Join(bin, "current")
	if target, err := os.Readlink(in.path(current)); err == nil {
		if _, err := os.Stat(in.path(filepath.Join(bin, target, CoreName))); err == nil {
			if target != sum {
				in.step(StepBinary, OutcomeRefused, "bin/current -> "+target+"; this binary is "+sum)
				dev := ""
				if buildstamp.Read() != nil {
					dev = " --allow-dev-binary"
				}
				return fmt.Errorf("%w: install writes a unit that starts bin/current, and this binary is the one "+
					"that sends the readiness that unit waits for; switch it first by running, as %s, "+
					"\"upgrade --binary %s --sha256 %s --state-dir %s%s\", then run install again",
					ErrBinaryMismatch, in.o.Account, in.o.Binary, sum, in.o.StateDir, dev)
			}
			in.step(StepBinary, OutcomeFound, "bin/current -> "+target)
			return nil
		}
	}
	dir := filepath.Join(bin, sum)
	if err := in.ensureDir(dir, 0o755, in.acct.UID, in.acct.GID, false); err != nil {
		return err
	}
	dst := filepath.Join(dir, CoreName)
	if have, _ := sha256sum.File(in.path(dst)); have != sum {
		if err := in.change("copy "+in.o.Binary+" to "+dst, func() error { return copyFile(in.o.Binary, in.path(dst), 0o755) }); err != nil {
			return err
		}
	}
	if !in.o.DryRun {
		if err := in.ensureOwner(in.path(dst), 0o755, in.acct.UID, in.acct.GID); err != nil {
			return err
		}
	}
	if err := in.change("point "+current+" at "+sum, func() error { return retarget(in.path(current), sum) }); err != nil {
		return err
	}
	in.step(StepBinary, OutcomeCreated, "bin/"+sum+"/"+CoreName)
	return nil
}

func retarget(link, target string) error {
	tmp := link + ".next"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (in *installer) config() error {
	p := filepath.Join(in.o.StateDir, "config.json")
	cfg := map[string]any{}
	if b, err := os.ReadFile(in.path(p)); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("install: %s is not valid JSON: %w", p, err)
		}
	}
	cfg["host-backend"] = string(hostsvc.BackendSystemdUser)
	cfg["state-dir"] = in.o.StateDir
	cfg["asset-cache"] = filepath.Join(in.o.StateDir, "runtime")
	if in.o.UnitPrefix != "" {
		cfg["unit-prefix"] = in.o.UnitPrefix
	} else {
		delete(cfg, "unit-prefix")
	}
	if in.o.Role == hostcontract.RoleBidder {
		if s, _ := cfg["profile"].(string); s == "" {
			cfg["profile"] = BidderProfile
		}
	}
	if in.o.Role == hostcontract.RoleGateway {
		for k, v := range GatewayListen {
			if s, _ := cfg[k].(string); s == "" {
				cfg[k] = v
			}
		}
	}
	cfg[OptReadyTimeout] = in.readyBudget().String()
	cfg[OptNetworkTimeout] = in.networkBudget().String()
	for k, v := range in.o.Settings {
		if v == "" || k == OptReadyTimeout || k == OptNetworkTimeout {
			continue
		}
		cfg[k] = v
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	changed, err := in.ensureFile(p, append(b, '\n'), 0o644, in.acct.UID, in.acct.GID)
	if err != nil {
		return err
	}
	if changed {
		in.cfgWrote = true
		in.step(StepConfig, OutcomeCreated, p+": host-backend "+string(hostsvc.BackendSystemdUser))
	} else {
		in.step(StepConfig, OutcomeFound, p)
	}
	return nil
}

func (in *installer) bringUp() error {
	if in.o.Role != hostcontract.RoleBidder {
		in.step(StepBringUp, OutcomeNotNeeded, "the gateway answers for itself")
		return nil
	}
	if in.o.DryRun {
		in.step(StepBringUp, OutcomeCreated, "would wait for the kcp slot of "+strings.Join(in.o.Handles, ", "))
		in.printf("would wait for the kcp slot of %s to answer /readyz\n", strings.Join(in.o.Handles, ", "))
		return nil
	}
	budget := in.readyBudget()
	details := make([]string, 0, len(in.o.Handles))
	skipped := make([]string, 0, len(in.o.Handles))
	for _, handle := range in.o.Handles {
		l := BidderLayout(in.o.StateDir, prefixOr(in.o.UnitPrefix), handle)
		slots, err := kcpserver.ReadSlots(in.path(l.SlotsFile()))
		if err != nil {
			skipped = append(skipped, handle+": "+err.Error())
			continue
		}
		slot, ok := slots.Slots[slots.Active]
		if !ok || slot.Port == 0 {
			skipped = append(skipped, handle+": the active slot records no port")
			continue
		}
		port := strconv.Itoa(slot.Port)
		ctx, cancel := context.WithTimeout(in.ctx, budget)
		reported := ""
		err = in.wait("waiting for the kcp slot of "+handle+" on 127.0.0.1:"+port, func() error {
			_, err := kcpserver.WaitReady(ctx, slot.Port, in.path(l.AdminKubeconfig()), time.Time{}, kcpserver.ProbePoll,
				func(_ int, last error) {
					msg := ""
					if last != nil {
						msg = last.Error()
					}
					if msg == "" || msg == reported {
						return
					}
					reported = msg
					in.printf("  %s\n", msg)
				})
			return err
		})
		cancel()
		if err != nil {
			in.step(StepBringUp, OutcomeRefused, err.Error())
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%w; raise --%s if this host's first start is slow, and read %s",
					err, OptReadyTimeout, journalHint(in.o.Account, in.acct.UID, CoreUnits(in.o.Role, in.o.UnitPrefix, in.o.Handles)...))
			}
			return err
		}
		details = append(details, handle+" on 127.0.0.1:"+port)
	}
	if len(details) == 0 {
		for _, handle := range in.o.Handles {
			l := BidderLayout(in.o.StateDir, prefixOr(in.o.UnitPrefix), handle)
			if _, err := os.Stat(in.path(l.SlotsFile())); err == nil {
				in.step(StepBringUp, OutcomeRefused, "the core wrote a kcp slot record with no active port: "+strings.Join(skipped, "; "))
				return fmt.Errorf("install: %s records no active kcp slot port; read %s",
					in.path(l.SlotsFile()), journalHint(in.o.Account, in.acct.UID, CoreUnits(in.o.Role, in.o.UnitPrefix, in.o.Handles)...))
			}
		}
		in.step(StepBringUp, OutcomeNotNeeded, "no handle published an active kcp slot port: "+strings.Join(skipped, "; "))
		return nil
	}
	if len(skipped) > 0 {
		in.step(StepBringUp, OutcomeRefused, "ready: "+strings.Join(details, ", ")+"; not ready: "+strings.Join(skipped, "; "))
		return fmt.Errorf("install: %s came up and %s did not; read %s", strings.Join(details, ", "),
			strings.Join(skipped, "; "), journalHint(in.o.Account, in.acct.UID,
				CoreUnits(in.o.Role, in.o.UnitPrefix, in.o.Handles)...))
	}
	in.step(StepBringUp, OutcomeFound, strings.Join(details, ", "))
	return nil
}

func (in *installer) chownTree(host string, uid, gid int) error {
	return filepath.WalkDir(in.path(host), func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return in.sys.Chown(p, uid, gid)
	})
}

func (in *installer) checkBudgets() error {
	for _, b := range []struct {
		opt, passed, why string
		min, max         time.Duration
	}{
		{OptReadyTimeout, in.o.ReadyTimeout, "that budget sizes the unit's start timeout and its watchdog, so " +
			"a longer one makes a wedged core wait longer to be restarted rather than giving a cold start more room",
			0, MaxReadyBudget},
		{OptNetworkTimeout, in.o.NetworkTimeout, "that budget is how long the guest image fetch may take, " +
			"a third of it, at least " + MinImageBudget.String() + ", is the search of the release base, and an " +
			"image that needs longer than either is better named with --" + OptGuestImg,
			MinImageBudget, MaxNetworkBudget},
	} {
		for _, v := range []struct{ src, val string }{
			{"--" + b.opt, b.passed},
			{"config.json " + b.opt, in.storedSetting(b.opt)},
		} {
			if v.val == "" {
				continue
			}
			d, err := time.ParseDuration(v.val)
			if err != nil {
				return fmt.Errorf("install: %s %q is not a duration: %w", v.src, v.val, err)
			}
			if d <= 0 {
				return fmt.Errorf("install: %s %q is not a positive duration", v.src, v.val)
			}
			if d < b.min {
				return fmt.Errorf("install: %s %q is under the minimum of %s, which is what the search of the "+
					"release base needs to list a bucket rather than report a slow one as a missing image",
					v.src, v.val, b.min)
			}
			if d > b.max {
				return fmt.Errorf("install: %s %q is over the maximum of %s: %s", v.src, v.val, b.max, b.why)
			}
			break
		}
	}
	return nil
}

func (in *installer) budget(opt, passed string, def time.Duration) time.Duration {
	for _, v := range []string{passed, in.storedSetting(opt)} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func (in *installer) readyBudget() time.Duration {
	return in.budget(OptReadyTimeout, in.o.ReadyTimeout, DefaultReadyBudget)
}

func (in *installer) networkBudget() time.Duration {
	return in.budget(OptNetworkTimeout, in.o.NetworkTimeout, DefaultNetworkBudget)
}

func (in *installer) waitStarted(um UserManager, unit string) error {
	ctx, cancel := context.WithTimeout(in.ctx, in.startBudget())
	defer cancel()
	flags := journalHint(in.o.Account, in.acct.UID, unit)
	var endedSince time.Time
	return in.wait("waiting for "+unit+" to finish starting", func() error {
		for {
			listed, err := um.List(ctx)
			if err != nil {
				return err
			}
			found := false
			for _, s := range listed {
				if s.Name != unit {
					continue
				}
				found = true
				if s.State == hostsvc.Active {
					return nil
				}
				if s.State != hostsvc.Activating {
					if endedSince.IsZero() {
						endedSince = time.Now()
					}
					if time.Since(endedSince) < StartEndedGrace {
						break
					}
					return fmt.Errorf("install: %s ended %s %s and stayed there for %s; read %s",
						unit, s.State, s.SubState, StartEndedGrace, flags)
				}
				endedSince = time.Time{}
			}
			if !found {
				return fmt.Errorf("install: %s is no longer known to systemd; read %s", unit, flags)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("install: %s is still starting after %s; read %s", unit, in.startBudget(), flags)
			case <-time.After(kcpserver.ProbePoll):
			}
		}
	})
}

func journalHint(account string, uid int, units ...string) string {
	flags := make([]string, 0, len(units))
	for _, u := range units {
		flags = append(flags, "-u "+u)
	}
	return fmt.Sprintf("runuser -u %s -- env XDG_RUNTIME_DIR=/run/user/%d journalctl --user %s", account, uid, strings.Join(flags, " "))
}

func (in *installer) startBudget() time.Duration {
	return StartTimeoutFor(in.readyBudget()) + StartJobGrace
}

func storeVerifies(store guestimage.Store, pin guestimage.Pin) bool {
	_, err := store.Verify(pin)
	return err == nil
}

func (in *installer) unitDir() string { return filepath.Join(in.acct.Home, UserUnitDir) }

func (in *installer) storedSetting(key string) string {
	b, err := os.ReadFile(in.path(filepath.Join(in.o.StateDir, "config.json")))
	if err != nil {
		return ""
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ""
	}
	s, _ := cfg[key].(string)
	return s
}

func (in *installer) guestImage() error {
	if in.o.Role != hostcontract.RoleBidder {
		in.step(StepGuestImage, OutcomeNotNeeded, "the gateway runs no guest")
		return nil
	}
	target := vmruntime.ImageTarget()
	images := filepath.Join(in.o.StateDir, "images")
	pin, err := guestimage.PinFor(target)
	if err != nil {
		in.step(StepGuestImage, OutcomeRefused, err.Error())
		return fmt.Errorf("install: this host cannot name the guest image it would boot: %w", err)
	}
	if err := pin.Valid(); err != nil {
		in.step(StepGuestImage, OutcomeRefused, err.Error())
		return fmt.Errorf("install: %w", err)
	}
	root := in.path(images)
	store := guestimage.Store{Root: root, Out: in.out(), TTY: progress.IsTTYWriter(in.out())}
	if in.o.DryRun {
		if dir := store.Dir(pin); fsx.Exists(dir) {
			in.step(StepGuestImage, OutcomeFound, dir)
			in.printf("the guest image for %s is already in %s\n", target, dir)
			return nil
		}
		in.step(StepGuestImage, OutcomeCreated, "would fetch the guest image for "+target+" into "+in.path(images))
		in.printf("would fetch the guest image for %s into %s\n", target, in.path(images))
		return nil
	}
	if err := in.ensureDir(images, 0o700, in.acct.UID, in.acct.GID, true); err != nil {
		return err
	}
	lookup := max(in.networkBudget()/3, MinImageBudget)
	lookupCtx, lookupCancel := context.WithTimeout(in.ctx, lookup)
	defer lookupCancel()
	dir := store.Dir(pin)
	if fsx.Exists(dir) && storeVerifies(store, pin) {
		if err := in.chownTree(images, in.acct.UID, in.acct.GID); err != nil {
			return err
		}
		in.step(StepGuestImage, OutcomeFound, dir)
		return nil
	}
	if fsx.Exists(dir) {
		in.printf("the stored guest image no longer matches its pin; fetching it again\n")
	}
	source := in.o.Settings[OptGuestImg]
	if source == "" {
		source = in.storedSetting(OptGuestImg)
	}
	if source == "" {
		if in.o.GuestImageBase == "" && in.o.GuestImageBaseResolved {
			in.step(StepGuestImage, OutcomeRefused, "no guest image source and the release base was cleared")
			return fmt.Errorf("install: no --%s and --%s is empty, so no image can be named; name one with --%s",
				OptGuestImg, OptGuestImgBase, OptGuestImg)
		}
		if in.o.GuestImageBase == "" {
			in.step(StepGuestImage, OutcomeNotNeeded, "the caller named no guest image source and no release base")
			return nil
		}
		version, commit := buildstamp.Identity()
		s, err := guestimagerelease.Source(lookupCtx, guestimagerelease.Options{Base: in.o.GuestImageBase, Target: target,
			Version: version, Commit: commit,
			ManifestSHA256: pin.Files[guestimage.ManifestName].SHA256})
		switch {
		case err == nil:
			source = s
		case errors.Is(err, guestimagerelease.ErrPinMismatch):
			in.step(StepGuestImage, OutcomeRefused, err.Error())
			return fmt.Errorf("%w; name a matching image with --%s, or build one with scripts/build-guest-image.sh", err, OptGuestImg)
		case errors.Is(err, guestimagerelease.ErrNotPublished):
			in.step(StepGuestImage, OutcomeRefused, err.Error())
			return fmt.Errorf("%w; a bidder with no guest image can provision nothing, so name one with --%s", err, OptGuestImg)
		default:
			in.step(StepGuestImage, OutcomeRefused, err.Error())
			if errors.Is(err, context.Canceled) {
				return err
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%w; raise --%s, of which this search is a third floored at %s, or name an "+
					"image with --%s", err, OptNetworkTimeout, MinImageBudget, OptGuestImg)
			}
			return fmt.Errorf("%w; name an image with --%s, or check that %s answers this host",
				err, OptGuestImg, in.o.GuestImageBase)
		}
	}
	if fsx.Exists(dir) {
		if err := removeImageTree(root, dir); err != nil {
			return err
		}
	}
	in.printf("fetching the guest image for %s from %s\n", target, source)
	ctx, cancel := context.WithTimeout(in.ctx, in.networkBudget())
	defer cancel()
	res, err := store.Ensure(ctx, pin, source)
	if err != nil {
		in.step(StepGuestImage, OutcomeRefused, err.Error())
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w; raise --%s if the image is large or the link slow, or name an image with --%s", err, OptNetworkTimeout, OptGuestImg)
		}
		return fmt.Errorf("%w; name a matching image with --%s, or build one with scripts/build-guest-image.sh", err, OptGuestImg)
	}
	outcome := OutcomeFound
	if res.Fetched {
		outcome = OutcomeCreated
	}
	if err := in.chownTree(images, in.acct.UID, in.acct.GID); err != nil {
		return err
	}
	in.step(StepGuestImage, outcome, res.Dir)
	return nil
}

func removeImageTree(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) ||
		strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("install: refusing to remove %s: it is not inside the image store %s", dir, root)
	}
	if len(filepath.Base(dir)) != guestimage.SHA256HexLen {
		return fmt.Errorf("install: refusing to remove %s: it is not named for a manifest sha256", dir)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("install: resolving the image store %s: %w", root, err)
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("install: resolving %s: %w", filepath.Dir(dir), err)
	}
	if under, err := filepath.Rel(realRoot, realDir); err != nil || under == ".." || filepath.IsAbs(under) ||
		strings.HasPrefix(under, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("install: refusing to remove %s: its directory resolves to %s, outside the image store %s", dir, realDir, realRoot)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("install: removing the unusable image at %s: %w", dir, err)
	}
	return nil
}

func (in *installer) unitFiles() error {
	units, err := RenderUnits(in.o.Role, in.o.StateDir, in.o.UnitPrefix, StartTimeoutFor(in.readyBudget()))
	if err != nil {
		return err
	}
	dir := in.unitDir()
	for _, d := range []string{filepath.Join(in.acct.Home, ".config"), filepath.Join(in.acct.Home, ".config/systemd"), dir} {
		if err := in.ensureDir(d, 0o755, in.acct.UID, in.acct.GID, false); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(units))
	for n := range units {
		names = append(names, n)
	}
	sort.Strings(names)
	any := false
	for _, n := range names {
		changed, err := in.ensureFile(filepath.Join(dir, n), units[n], 0o644, in.acct.UID, in.acct.GID)
		if err != nil {
			return err
		}
		in.rec.addUnitFile(filepath.Join(dir, n))
		if changed {
			in.noteWritten(n)
		}
		any = any || changed
	}
	boot, bootChanged, err := in.bootstrapFiles(dir)
	if err != nil {
		return err
	}
	names = append(names, boot...)
	any = any || bootChanged
	if any {
		in.step(StepUnitFiles, OutcomeCreated, strings.Join(names, " "))
	} else {
		in.step(StepUnitFiles, OutcomeFound, strings.Join(names, " "))
	}
	return nil
}

func (in *installer) noteWritten(name string) {
	if in.wrote == nil {
		in.wrote = map[string]bool{}
	}
	in.wrote[name] = true
}

func coreTemplate(unit string) string {
	at := strings.Index(unit, "@")
	dot := strings.LastIndex(unit, ".")
	if at < 0 || dot < at {
		return ""
	}
	return unit[:at+1] + "." + unit[dot+1:]
}

func (in *installer) unitChanged(unit string) bool {
	if in.cfgWrote || in.wrote[unit] || in.wrote[coreTemplate(unit)] {
		return true
	}
	for p := range in.wrote {
		if strings.HasPrefix(p, unit+".d/") {
			return true
		}
	}
	return false
}

func (in *installer) linger() error {
	p := filepath.Join(LingerDir, in.o.Account)
	if _, err := os.Stat(in.path(p)); err == nil {
		in.step(StepLinger, OutcomeFound, p)
		return nil
	}
	if err := in.change("loginctl enable-linger "+in.o.Account+" (user@"+in.uidLabel()+" first starts here)", func() error {
		return in.sys.SetLinger(in.ctx, in.acct, true)
	}); err != nil {
		return err
	}
	in.step(StepLinger, OutcomeCreated, p)
	return nil
}

func (in *installer) enable() error {
	units := CoreUnits(in.o.Role, in.o.UnitPrefix, in.o.Handles)
	if in.o.DryRun && !in.known {
		for _, u := range units {
			in.rep.Changes = append(in.rep.Changes, "enable --now "+u)
			in.printf("would enable --now %s as %s\n", u, in.o.Account)
		}
		return nil
	}
	if in.o.DryRun {
		if _, err := os.Stat(in.path(filepath.Join(LingerDir, in.o.Account))); err != nil {
			for _, u := range units {
				in.rep.Changes = append(in.rep.Changes, "enable --now "+u)
				in.printf("would enable --now %s as %s\n", u, in.o.Account)
			}
			return nil
		}
	}
	um, err := in.sys.User(in.ctx, in.acct)
	if err != nil {
		return fmt.Errorf("install: reaching user@%d: %w", in.acct.UID, err)
	}
	defer um.Close()
	actx, acancel := context.WithTimeout(in.ctx, in.startBudget())
	defer acancel()
	states := map[string]hostsvc.UnitStatus{}
	listed, err := um.List(actx)
	if err != nil {
		return err
	}
	for _, s := range listed {
		states[s.Name] = s
	}
	var todo, starting, restart []string
	for _, u := range units {
		_, linkErr := os.Lstat(in.path(filepath.Join(in.unitDir(), WantsDir, u)))
		st := states[u]
		switch {
		case linkErr != nil || (st.State != hostsvc.Active && st.State != hostsvc.Activating):
			todo = append(todo, u)
		case in.unitChanged(u):
			restart = append(restart, u)
		case st.State == hostsvc.Activating:
			starting = append(starting, u)
		}
	}
	in.rec.addHandles(in.o.Handles)
	if err := in.saveRecord(); err != nil {
		return err
	}
	if len(todo) == 0 && len(starting) == 0 && len(restart) == 0 {
		in.step(StepUnits, OutcomeFound, strings.Join(units, " "))
		return nil
	}
	if err := in.change("systemctl --user daemon-reload as "+in.o.Account, func() error { return um.Reload(actx) }); err != nil {
		return err
	}
	if err := in.enableBootstrap(actx, um); err != nil {
		return err
	}
	for _, u := range todo {
		sctx, cancel := context.WithTimeout(in.ctx, in.startBudget())
		err := in.changeWait("enable --now "+u+" as "+in.o.Account, func() error { return um.EnableNow(sctx, []string{u}) })
		cancel()
		if err != nil {
			in.step(StepUnits, OutcomeRefused, u+": "+err.Error())
			return fmt.Errorf("%w; read %s", err, journalHint(in.o.Account, in.acct.UID, u))
		}
	}
	for _, u := range restart {
		sctx, cancel := context.WithTimeout(in.ctx, in.startBudget())
		err := in.changeWait("restart "+u+" as "+in.o.Account+" (its unit file or its config.json changed)", func() error {
			return um.Restart(sctx, []string{u})
		})
		cancel()
		if err != nil {
			in.step(StepUnits, OutcomeRefused, u+": "+err.Error())
			return fmt.Errorf("%w; read %s", err, journalHint(in.o.Account, in.acct.UID, u))
		}
	}
	for _, u := range starting {
		if err := in.waitStarted(um, u); err != nil {
			in.step(StepUnits, OutcomeRefused, u+": "+err.Error())
			return err
		}
	}
	outcome := OutcomeCreated
	if len(todo) == 0 && len(starting) == 0 {
		outcome = OutcomeUpdated
	}
	in.step(StepUnits, outcome, strings.Join(units, " "))
	return nil
}

func (in *installer) checkHost() error {
	exe := filepath.Join(in.o.StateDir, "bin", "current", CoreName)
	desc := "run check-host -role " + string(in.o.Role) + " as " + in.o.Account
	if in.o.DryRun {
		in.printf("would %s\n", desc)
		return nil
	}
	in.printf("%s\n", desc)
	cctx, cancel := context.WithTimeout(in.ctx, HostCheckBudget)
	defer cancel()
	if err := in.sys.CheckHost(cctx, in.acct, exe, in.o.Role, in.o.StateDir); err != nil {
		in.rep.Steps = append(in.rep.Steps, StepResult{Name: StepCheckHost, Outcome: OutcomeRefused, Detail: err.Error()})
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("install: %s did not finish within %s, and it prints what it found only at the end: %w",
				desc, HostCheckBudget, err)
		}
		return fmt.Errorf("install: %s: %w; fix what it names and re-run install, which completes from here", desc, err)
	}
	in.rep.Steps = append(in.rep.Steps, StepResult{Name: StepCheckHost, Outcome: OutcomeFound, Detail: "the host contract holds"})
	return nil
}

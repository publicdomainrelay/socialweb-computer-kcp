package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc/treeaudit"
)

type UninstallOptions struct {
	Role    hostcontract.Role
	Account string
	Purge   bool
	Force   bool
	Root    string
	Staged  bool
	Out     io.Writer
}

func OwnsUnit(prefix, name string) bool {
	if prefix != "" && prefix != RuntimePrefix {
		return strings.HasPrefix(name, prefix) || name == strings.TrimSuffix(prefix, "-")+".slice"
	}
	return strings.HasPrefix(name, RuntimePrefix) || name == SliceUnit || strings.HasPrefix(name, RuntimeSliceTop+"-")
}

type uninstaller struct {
	installer
	uo UninstallOptions
}

func Uninstall(ctx context.Context, uo UninstallOptions, sys System) (Report, error) {
	rep := Report{}
	if uo.Account == "" {
		uo.Account = DefaultAccount(uo.Role)
	}
	uo.Root = cmp.Or(uo.Root, "/")
	recPath := filepath.Join(uo.Root, RecordPath(uo.Account))
	rec, err := loadRecord(recPath)
	if errors.Is(err, fs.ErrNotExist) {
		return rep, fmt.Errorf("%w: %s is missing, so nothing is known to have been created by install", ErrNoRecord, RecordPath(uo.Account))
	}
	if err != nil {
		return rep, err
	}
	if rec.Role != uo.Role {
		return rep, fmt.Errorf("uninstall: %s was installed as a %s, not a %s", uo.Account, rec.Role, uo.Role)
	}
	u := &uninstaller{installer: installer{ctx: ctx, o: Options{Role: uo.Role, Account: uo.Account, StateDir: rec.StateDir, Root: uo.Root, Staged: uo.Staged, Out: uo.Out},
		sys: sys, rep: &rep, rec: rec, recPath: recPath}, uo: uo}
	a, ok, err := sys.LookupAccount(uo.Account)
	if err != nil {
		return rep, err
	}
	if ok {
		u.acct, u.known = a, true
	}
	var um UserManager
	if u.known {
		mgRoot := uo.Root
		if uo.Staged {
			mgRoot = "/"
		}
		if _, _, notRunning := hostcontract.ManagerGroups(mgRoot, a.UID); notRunning == "" {
			if um, err = sys.User(ctx, a); err != nil {
				return rep, fmt.Errorf("uninstall: reaching user@%d: %w", a.UID, err)
			}
			defer um.Close()
		}
	}
	var units []hostsvc.UnitStatus
	if um != nil {
		if units, err = um.List(ctx); err != nil {
			return rep, err
		}
		var vms []string
		for _, s := range units {
			if OwnsUnit(rec.UnitPrefix, s.Name) && treeaudit.IsVMUnit(s.Name) && (s.State == hostsvc.Active || s.State == hostsvc.Activating || s.State == hostsvc.Reloading) {
				vms = append(vms, s.Name)
			}
		}
		if len(vms) > 0 && !uo.Force {
			return rep, fmt.Errorf("%w: %s; stop them through their ComputeVMs first, or pass --force", ErrVMsRunning, strings.Join(vms, ", "))
		}
	}
	if uo.Purge {
		fmt.Fprintf(u.out(), "uninstall --purge removes the state dir %s (the vault, the kcp store, the keys) and, where install created it, the account %s\n",
			rec.StateDir, uo.Account)
	}
	for _, step := range []func([]hostsvc.UnitStatus, UserManager) error{u.stopUnits, u.removeUnitFiles, u.lingerOff, u.removeDropIn,
		u.removeAppArmor, u.removeSubids, u.removeKVM, u.removePassword, u.purge} {
		if err := step(units, um); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (u *uninstaller) stopUnits(units []hostsvc.UnitStatus, um UserManager) error {
	if um == nil {
		return nil
	}
	core := CoreUnits(u.rec.Role, u.rec.UnitPrefix, u.rec.Handles)
	if err := u.change("disable --now "+strings.Join(core, " ")+" as "+u.uo.Account, func() error { return um.DisableStop(u.ctx, core) }); err != nil {
		return err
	}
	isCore := map[string]bool{}
	for _, c := range core {
		isCore[c] = true
	}
	var rest []string
	for _, s := range units {
		if OwnsUnit(u.rec.UnitPrefix, s.Name) && !isCore[s.Name] && s.State != hostsvc.Inactive && s.State != hostsvc.NotFound {
			rest = append(rest, s.Name)
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		return strings.HasSuffix(rest[j], ".slice") && !strings.HasSuffix(rest[i], ".slice") || (strings.HasSuffix(rest[i], ".slice") == strings.HasSuffix(rest[j], ".slice") && rest[i] < rest[j])
	})
	for _, n := range rest {
		if err := u.change("stop "+n, func() error { return um.Stop(u.ctx, n) }); err != nil {
			return err
		}
	}
	return nil
}

func (u *uninstaller) remove(host string) error {
	full := u.path(host)
	if _, err := os.Lstat(full); err != nil {
		return nil
	}
	return u.change("remove "+host, func() error { return os.Remove(full) })
}

func isDropIn(f string) bool { return strings.HasSuffix(filepath.Dir(f), ".d") }

func isLinkDir(name string) bool {
	return strings.HasSuffix(name, ".wants") || strings.HasSuffix(name, ".requires") || strings.HasSuffix(name, ".upholds")
}

func (u *uninstaller) recordedUnits() (map[string]bool, []string, []string) {
	names := map[string]bool{}
	var own []string
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, f := range u.rec.UnitFiles {
		if isDropIn(f) || isLinkDir(filepath.Base(filepath.Dir(f))) {
			continue
		}
		names[filepath.Base(f)] = true
		add(u.path(filepath.Dir(f)))
	}
	own = append(own, dirs...)
	if u.uo.Staged {
		if c, err := os.UserConfigDir(); err == nil {
			add(filepath.Join(c, "systemd", "user"))
		}
		if r := OwnRuntimeDir(); r != "" {
			add(filepath.Join(r, "systemd", "user"))
		}
	}
	return names, dirs, own
}

func ownedLink(p string, own []string) bool {
	cur := p
	for range 4 {
		t, err := os.Readlink(cur)
		if err != nil {
			return false
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(cur), t)
		}
		t = filepath.Clean(t)
		for _, d := range own {
			if within(t, d) {
				return true
			}
		}
		if _, err := os.Lstat(t); errors.Is(err, fs.ErrNotExist) {
			return true
		}
		cur = t
	}
	return false
}

func (u *uninstaller) removeLink(p string, own []string) (bool, error) {
	st, err := os.Lstat(p)
	if err != nil || st.Mode()&fs.ModeSymlink == 0 || !ownedLink(p, own) {
		return false, nil
	}
	return true, u.change("remove "+p, func() error { return os.Remove(p) })
}

func (u *uninstaller) sweepUnitLinks(names map[string]bool, dirs, own []string) (bool, error) {
	swept := false
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() && isLinkDir(e.Name()) {
				subs, err := os.ReadDir(p)
				if err != nil {
					continue
				}
				left := len(subs)
				for _, s := range subs {
					if !names[s.Name()] {
						continue
					}
					removed, err := u.removeLink(filepath.Join(p, s.Name()), own)
					if err != nil {
						return swept, err
					}
					if removed {
						swept, left = true, left-1
					}
				}
				if left == 0 && len(subs) > 0 {
					os.Remove(p)
				}
				continue
			}
			if names[e.Name()] {
				removed, err := u.removeLink(p, own)
				if err != nil {
					return swept, err
				}
				swept = swept || removed
			}
		}
	}
	return swept, nil
}

func (u *uninstaller) removeUnitFiles(units []hostsvc.UnitStatus, um UserManager) error {
	names, dirs, own := u.recordedUnits()
	for _, f := range u.rec.UnitFiles {
		if filepath.Base(filepath.Dir(f)) != WantsDir && !strings.HasSuffix(filepath.Dir(f), ".d") {
			link := filepath.Join(filepath.Dir(f), WantsDir, filepath.Base(f))
			if _, err := os.Lstat(u.path(link)); err == nil {
				if err := u.remove(link); err != nil {
					return err
				}
			}
		}
		if err := u.remove(f); err != nil {
			return err
		}
		if isDropIn(f) {
			os.Remove(u.path(filepath.Dir(f)))
		}
	}
	swept, err := u.sweepUnitLinks(names, dirs, own)
	if err != nil {
		return err
	}
	u.rec.UnitFiles = nil
	if swept && um != nil {
		return u.change("systemctl --user daemon-reload as "+u.uo.Account, func() error { return um.Reload(u.ctx) })
	}
	return nil
}

func (u *uninstaller) lingerOff(units []hostsvc.UnitStatus, um UserManager) error {
	if !u.rec.created(StepLinger) || !u.known {
		return nil
	}
	return u.change("loginctl disable-linger "+u.uo.Account, func() error { return u.sys.SetLinger(u.ctx, u.acct, false) })
}

func (u *uninstaller) removeDropIn(units []hostsvc.UnitStatus, um UserManager) error {
	if !u.rec.created(StepDelegation) || u.rec.UID < 0 {
		return nil
	}
	dir := fmt.Sprintf("/etc/systemd/system/user@%d.service.d", u.rec.UID)
	if err := u.remove(filepath.Join(dir, DropInName)); err != nil {
		return err
	}
	u.removeCreatedDirs(func(d string) bool { return d == dir })
	return u.change("systemctl daemon-reload (system manager)", func() error { return u.sys.ReloadSystemManager(u.ctx) })
}

func (u *uninstaller) removeAppArmor(units []hostsvc.UnitStatus, um UserManager) error {
	if !u.rec.created(StepAppArmor) {
		return nil
	}
	if hostcontract.AppArmorProfileLoaded(u.read(AppArmorLoaded), hostcontract.AppArmorProfileName) {
		if err := u.change("apparmor_parser -R "+AppArmorPath, func() error { return u.sys.UnloadAppArmor(u.path(AppArmorPath)) }); err != nil {
			return err
		}
	}
	return u.remove(AppArmorPath)
}

func (u *uninstaller) removeSubids(units []hostsvc.UnitStatus, um UserManager) error {
	for _, f := range []struct{ step, host string }{{StepSubuid, "/etc/subuid"}, {StepSubgid, "/etc/subgid"}} {
		line := u.rec.SubidLines[f.host]
		if !u.rec.created(f.step) || line == "" {
			continue
		}
		content := u.read(f.host)
		i := strings.Index(content, line)
		if i < 0 || (i > 0 && content[i-1] != '\n') {
			continue
		}
		next := content[:i] + content[i+len(line):]
		full := u.path(f.host)
		mode := os.FileMode(0o644)
		if st, err := os.Stat(full); err == nil {
			mode = st.Mode().Perm()
		}
		if err := u.change("remove "+strings.TrimSpace(line)+" from "+f.host, func() error { return writeAtomic(full, []byte(next), mode) }); err != nil {
			return err
		}
	}
	return nil
}

func (u *uninstaller) removeKVM(units []hostsvc.UnitStatus, um UserManager) error {
	if !u.rec.created(StepKVM) || !u.known {
		return nil
	}
	if _, member := hostcontract.GroupMembership(u.read("/etc/group"), KVMGroup, u.uo.Account); !member {
		return nil
	}
	return u.change("remove "+u.uo.Account+" from group "+KVMGroup, func() error { return u.sys.RemoveFromGroup(u.uo.Account, KVMGroup) })
}

func (u *uninstaller) removePassword(units []hostsvc.UnitStatus, um UserManager) error {
	switch {
	case u.rec.created(StepPassword):
		if err := u.remove(NodePassword); err != nil {
			return err
		}
		u.removeCreatedDirs(func(d string) bool { return strings.HasPrefix(NodePassword, d+"/") })
	case u.rec.Steps[StepPassword] == OutcomeUpdated:
		if prev, ok := u.rec.PrevOwner[NodePassword]; ok {
			full := u.path(NodePassword)
			return u.change(fmt.Sprintf("chown %d:%d %s (its owner before install)", prev[0], prev[1], NodePassword), func() error {
				return u.sys.Chown(full, prev[0], prev[1])
			})
		}
	}
	return nil
}

func (u *uninstaller) removeCreatedDirs(match func(string) bool) {
	dirs := append([]string(nil), u.rec.CreatedDirs...)
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if !match(d) {
			continue
		}
		full := u.path(d)
		entries, err := os.ReadDir(full)
		if err != nil || len(entries) > 0 {
			continue
		}
		_ = u.change("remove directory "+d, func() error { return os.Remove(full) })
	}
}

func (u *uninstaller) purge(units []hostsvc.UnitStatus, um UserManager) error {
	if !u.uo.Purge {
		kept := &Record{Role: u.rec.Role, Account: u.rec.Account, UID: u.rec.UID, StateDir: u.rec.StateDir, UnitPrefix: u.rec.UnitPrefix,
			Steps: map[string]string{StepAccount: u.rec.Steps[StepAccount], StepStateDir: u.rec.Steps[StepStateDir]}}
		for _, d := range u.rec.CreatedDirs {
			if d == u.rec.StateDir || strings.HasPrefix(u.rec.StateDir, d+"/") {
				kept.CreatedDirs = append(kept.CreatedDirs, d)
			}
		}
		u.rec = kept
		u.installer.rec = kept
		return u.saveRecord()
	}
	if err := purgeableState(u.rec.StateDir); err != nil {
		return err
	}
	state := u.path(u.rec.StateDir)
	if _, err := os.Lstat(state); err == nil {
		if err := u.change("remove the state dir "+u.rec.StateDir, func() error { return removeTree(state) }); err != nil {
			return err
		}
	}
	if u.rec.created(StepAccount) && u.known {
		if err := u.change("delete account "+u.uo.Account, func() error { return u.sys.DeleteAccount(u.uo.Account) }); err != nil {
			return err
		}
	}
	u.removeCreatedDirs(func(string) bool { return true })
	if err := u.change("remove the install record "+RecordPath(u.uo.Account), func() error { return os.Remove(u.recPath) }); err != nil {
		return err
	}
	u.removeCreatedDirs(func(string) bool { return true })
	return nil
}

func purgeableState(state string) error {
	clean := filepath.Clean(state)
	if !filepath.IsAbs(clean) || strings.Count(clean, "/") < 2 {
		return fmt.Errorf("uninstall: the recorded state dir %q is not an absolute path two levels deep; --purge refuses to remove it", state)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == clean {
		return fmt.Errorf("uninstall: the recorded state dir %s is the invoking user's home; --purge refuses to remove it", state)
	}
	return nil
}

func writableTree(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d == nil || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if st, serr := os.Lstat(p); serr == nil {
			mode := st.Mode().Perm() | 0o600
			if d.IsDir() {
				mode |= 0o700
			}
			if mode != st.Mode().Perm() {
				os.Chmod(p, mode)
			}
		}
		return nil
	})
}

func removeTree(root string) error {
	writableTree(root)
	if err := os.RemoveAll(root); err != nil {
		writableTree(root)
		if err := os.RemoveAll(root); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("uninstall: %s is still there after its removal", root)
	}
	return nil
}

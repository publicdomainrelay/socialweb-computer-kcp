//go:build linux

package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
)

const stagedOwners = ".staged-owners.json"

type stagedSystem struct {
	root string
	out  io.Writer
}

func OwnRuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}

func NewStagedSystem(root string, out io.Writer) (System, error) {
	if out == nil {
		out = io.Discard
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("install: a staged host belongs to the invoking user's own manager; never root")
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		return nil, err
	}
	return stagedSystem{root: root, out: out}, nil
}

func (s stagedSystem) say(format string, args ...any) {
	fmt.Fprintf(s.out, "staged: "+format+"\n", args...)
}

func (s stagedSystem) file(host string) string { return filepath.Join(s.root, host) }

func (s stagedSystem) lines(host string) []string {
	b, _ := os.ReadFile(s.file(host))
	var out []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (s stagedSystem) writeLines(host string, lines []string) error {
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	return writeAtomic(s.file(host), []byte(body), 0o644)
}

func (s stagedSystem) LookupAccount(name string) (Account, bool, error) {
	for _, l := range s.lines("/etc/passwd") {
		f := strings.Split(l, ":")
		if len(f) < 7 || f[0] != name {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			return Account{}, false, fmt.Errorf("install: %s in the staged passwd has a non-numeric uid or gid", name)
		}
		return Account{Name: name, UID: uid, GID: gid, Home: f[5]}, true, nil
	}
	return Account{}, false, nil
}

func (s stagedSystem) CreateAccount(name, home string) error {
	uid, gid := os.Getuid(), os.Getgid()
	s.say("account %s is the invoking user (uid %d) in %s/etc/passwd", name, uid, s.root)
	if err := s.writeLines("/etc/passwd", append(s.lines("/etc/passwd"),
		fmt.Sprintf("%s:x:%d:%d:socialweb-computer:%s:/usr/sbin/nologin", name, uid, gid, home))); err != nil {
		return err
	}
	return s.writeLines("/etc/group", append(s.lines("/etc/group"), fmt.Sprintf("%s:x:%d:", name, gid)))
}

func (s stagedSystem) DeleteAccount(name string) error {
	for _, host := range []string{"/etc/passwd", "/etc/group"} {
		var keep []string
		for _, l := range s.lines(host) {
			if !strings.HasPrefix(l, name+":") {
				keep = append(keep, l)
			}
		}
		if err := s.writeLines(host, keep); err != nil {
			return err
		}
	}
	return nil
}

func (s stagedSystem) editGroup(account, group string, add bool) error {
	ls := s.lines("/etc/group")
	for i, l := range ls {
		f := strings.Split(l, ":")
		if len(f) != 4 || f[0] != group {
			continue
		}
		var members []string
		for m := range strings.SplitSeq(f[3], ",") {
			if m != "" && m != account {
				members = append(members, m)
			}
		}
		if add {
			members = append(members, account)
		}
		f[3] = strings.Join(members, ",")
		ls[i] = strings.Join(f, ":")
		return s.writeLines("/etc/group", ls)
	}
	return fmt.Errorf("install: no %s group in the staged %s/etc/group", group, s.root)
}

func (s stagedSystem) AddToGroup(account, group string) error {
	return s.editGroup(account, group, true)
}

func (s stagedSystem) RemoveFromGroup(account, group string) error {
	return s.editGroup(account, group, false)
}

func (s stagedSystem) owners() map[string][2]int {
	m := map[string][2]int{}
	if b, err := os.ReadFile(filepath.Join(s.root, stagedOwners)); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (s stagedSystem) Chown(path string, uid, gid int) error {
	if uid == os.Getuid() && gid == os.Getgid() {
		m := s.owners()
		if _, ok := m[path]; ok {
			delete(m, path)
			b, _ := json.Marshal(m)
			if err := writeAtomic(filepath.Join(s.root, stagedOwners), b, 0o600); err != nil {
				return err
			}
		}
		return os.Lchown(path, uid, gid)
	}
	m := s.owners()
	m[path] = [2]int{uid, gid}
	b, _ := json.Marshal(m)
	s.say("chown %d:%d %s recorded, not applied", uid, gid, path)
	return writeAtomic(filepath.Join(s.root, stagedOwners), b, 0o600)
}

func (s stagedSystem) Owner(path string) (int, int, error) {
	if o, ok := s.owners()[path]; ok {
		if _, err := os.Lstat(path); err == nil {
			return o[0], o[1], nil
		}
	}
	return liveSystem{}.Owner(path)
}

func (s stagedSystem) ReloadSystemManager(ctx context.Context) error {
	s.say("the system manager is not reloaded")
	return nil
}

func (s stagedSystem) LoadAppArmor(path string) error {
	s.say("apparmor_parser -r %s is not run", path)
	return nil
}

func (s stagedSystem) UnloadAppArmor(path string) error {
	s.say("apparmor_parser -R %s is not run", path)
	return nil
}

func (s stagedSystem) SetLinger(ctx context.Context, a Account, on bool) error {
	p := s.file(filepath.Join(LingerDir, a.Name))
	if !on {
		return os.Remove(p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	s.say("linger is the invoking user's own; %s stands for it", p)
	return os.WriteFile(p, nil, 0o644)
}

func (s stagedSystem) User(ctx context.Context, a Account) (UserManager, error) {
	if a.UID != os.Getuid() {
		return nil, fmt.Errorf("install: the staged account %s is uid %d, not the invoking user", a.Name, a.UID)
	}
	um, err := liveSystem{ctx: ctx}.User(ctx, a)
	if err != nil {
		return nil, err
	}
	return &stagedUser{ctx: ctx, liveUser: um.(*liveUser), units: filepath.Join(a.Home, UserUnitDir),
		runtime: filepath.Join(OwnRuntimeDir(), "systemd", "user")}, nil
}

func (s stagedSystem) CheckHost(ctx context.Context, a Account, binary string, role hostcontract.Role, stateDir string) error {
	cmd := exec.CommandContext(ctx, binary, "check-host", "-role", string(role), "-state-dir", stateDir)
	cmd.Dir = stateDir
	cmd.Env = append(os.Environ(), hostcontract.StagedRootEnv+"="+s.root)
	cmd.Stdout, cmd.Stderr = s.out, s.out
	return cmd.Run()
}

type stagedUser struct {
	ctx context.Context
	*liveUser
	units   string
	runtime string
}

func (u *stagedUser) link(link, target string) error {
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	os.Remove(link)
	return os.Symlink(target, link)
}

func (u *stagedUser) prune() bool {
	pruned := false
	filepath.WalkDir(u.runtime, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		t, err := os.Readlink(p)
		if err != nil {
			return nil
		}
		if within(t, u.runtime) {
			if t, err = os.Readlink(t); err != nil {
				return nil
			}
		}
		if !within(t, u.units) {
			return nil
		}
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			os.Remove(p)
			pruned = true
		}
		return nil
	})
	return pruned
}

func (u *stagedUser) sync() error {
	entries, err := os.ReadDir(u.units)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == WantsDir {
			continue
		}
		if err := u.link(filepath.Join(u.runtime, e.Name()), filepath.Join(u.units, e.Name())); err != nil {
			return err
		}
	}
	u.prune()
	return nil
}

func (u *stagedUser) Reload(ctx context.Context) error {
	if err := u.sync(); err != nil {
		return err
	}
	return u.liveUser.Reload(ctx)
}

func (u *stagedUser) fileOf(unit string) string {
	if _, err := os.Stat(filepath.Join(u.units, unit)); err == nil {
		return unit
	}
	if at := strings.Index(unit, "@"); at > 0 {
		return unit[:at+1] + unit[strings.LastIndex(unit, "."):]
	}
	return unit
}

func (u *stagedUser) Enable(ctx context.Context, units []string) error {
	for _, unit := range units {
		f := u.fileOf(unit)
		if err := u.link(filepath.Join(u.units, WantsDir, unit), filepath.Join("..", f)); err != nil {
			return err
		}
		if err := u.link(filepath.Join(u.runtime, WantsDir, unit), filepath.Join(u.runtime, f)); err != nil {
			return err
		}
	}
	return u.Reload(ctx)
}

func (u *stagedUser) EnableNow(ctx context.Context, units []string) error {
	if err := u.Enable(ctx, units); err != nil {
		return err
	}
	for _, unit := range units {
		if err := waitJob(ctx, unit, func(ch chan<- string) error {
			_, err := u.conn.StartUnitContext(ctx, unit, "replace", ch)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (u *stagedUser) DisableStop(ctx context.Context, units []string) error {
	for _, unit := range units {
		if err := u.Stop(ctx, unit); err != nil {
			return err
		}
		os.Remove(filepath.Join(u.units, WantsDir, unit))
		os.Remove(filepath.Join(u.runtime, WantsDir, unit))
	}
	return nil
}

func (u *stagedUser) Close() {
	if u.prune() {
		os.Remove(filepath.Join(u.runtime, WantsDir))
		os.Remove(u.runtime)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), jobTimeout)
		u.liveUser.Reload(ctx)
		cancel()
	}
	u.liveUser.Close()
}

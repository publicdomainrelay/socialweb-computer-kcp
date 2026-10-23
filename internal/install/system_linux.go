//go:build linux

package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

const (
	managerSocketWait = 30 * time.Second
	jobTimeout        = 2 * time.Minute
	restartRecovery   = 60 * time.Second
	restartPollEvery  = 500 * time.Millisecond
	hostPath          = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

type liveSystem struct {
	out io.Writer
	ctx context.Context
}

func NewSystem(ctx context.Context, out io.Writer) (System, error) {
	if out == nil {
		out = io.Discard
	}
	return liveSystem{out: out, ctx: ctx}, nil
}

func (s liveSystem) run(cmd *exec.Cmd) error {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = CommandBudget
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(cmd.Path), err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return ranError(cmd, err, out.Bytes())
	case <-s.ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s: %w while it ran", filepath.Base(cmd.Path), s.ctx.Err())
	case <-time.After(CommandBudget):
		select {
		case err := <-done:
			return ranError(cmd, err, out.Bytes())
		default:
		}
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s: still running after %s, so it was killed: %s",
			filepath.Base(cmd.Path), CommandBudget, out.Bytes())
	}
}

func lookupWithin(cancel <-chan struct{}, what string, f func() (*user.User, error)) (*user.User, error) {
	type answer struct {
		u   *user.User
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		u, err := f()
		ch <- answer{u, err}
	}()
	select {
	case a := <-ch:
		return a.u, a.err
	case <-cancel:
		return nil, fmt.Errorf("%s: %w while the name service was asked", what, context.Canceled)
	case <-time.After(CommandBudget):
		return nil, fmt.Errorf("%s: the name service did not answer within %s", what, CommandBudget)
	}
}

func groupIDsWithin(cancel <-chan struct{}, u *user.User) ([]string, error) {
	type answer struct {
		ids []string
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		ids, err := u.GroupIds()
		ch <- answer{ids, err}
	}()
	select {
	case a := <-ch:
		return a.ids, a.err
	case <-cancel:
		return nil, fmt.Errorf("%s: %w while the name service was asked", u.Username, context.Canceled)
	case <-time.After(CommandBudget):
		return nil, fmt.Errorf("%s: the name service did not answer within %s", u.Username, CommandBudget)
	}
}

func ranError(cmd *exec.Cmd, err error, out []byte) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w: %s", filepath.Base(cmd.Path), err, out)
}

func (s liveSystem) LookupAccount(name string) (Account, bool, error) {
	u, err := lookupWithin(s.ctx.Done(), name, func() (*user.User, error) { return user.Lookup(name) })
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return Account{}, false, nil
		}
		return Account{}, false, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return Account{}, false, fmt.Errorf("install: %s has a non-numeric uid or gid", name)
	}
	return Account{Name: name, UID: uid, GID: gid, Home: u.HomeDir}, true, nil
}

func nologin() string {
	for _, p := range []string{"/usr/sbin/nologin", "/sbin/nologin"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/bin/false"
}

func (s liveSystem) CreateAccount(name, home string) error {
	return s.run(exec.Command("useradd", "--system", "--user-group", "--no-create-home", "--home-dir", home,
		"--shell", nologin(), "--comment", "socialweb-computer", name))
}

func (s liveSystem) DeleteAccount(name string) error {
	return s.run(exec.Command("userdel", name))
}

func (s liveSystem) AddToGroup(account, group string) error {
	return s.run(exec.Command("gpasswd", "--add", account, group))
}

func (s liveSystem) RemoveFromGroup(account, group string) error {
	return s.run(exec.Command("gpasswd", "--delete", account, group))
}

func (liveSystem) Chown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

func (liveSystem) Owner(path string) (int, int, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("install: no owner for %s", path)
	}
	return int(sys.Uid), int(sys.Gid), nil
}

func (liveSystem) ReloadSystemManager(ctx context.Context) error {
	conn, err := sd.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.ReloadContext(ctx)
}

func (s liveSystem) LoadAppArmor(path string) error {
	return s.run(exec.Command("apparmor_parser", "--replace", path))
}

func (s liveSystem) UnloadAppArmor(path string) error {
	return s.run(exec.Command("apparmor_parser", "--remove", path))
}

func (liveSystem) SetLinger(ctx context.Context, a Account, on bool) error {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Object("org.freedesktop.login1", "/org/freedesktop/login1").
		CallWithContext(ctx, "org.freedesktop.login1.Manager.SetUserLinger", 0, uint32(a.UID), on, false).Err
}

func (liveSystem) User(ctx context.Context, a Account) (UserManager, error) {
	socket := filepath.Join("/run/user", strconv.Itoa(a.UID), "systemd", "private")
	wait, cancel := context.WithTimeout(ctx, managerSocketWait)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		select {
		case <-wait.Done():
			return nil, fmt.Errorf("user@%d's private socket %s did not appear: %w", a.UID, socket, wait.Err())
		case <-tick.C:
		}
	}
	conn, err := sd.NewConnection(func() (*dbus.Conn, error) {
		c, err := dbus.Dial("unix:path=" + socket)
		if err != nil {
			return nil, err
		}
		if err := c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	return &liveUser{conn: conn}, nil
}

type liveUser struct {
	conn *sd.Conn
}

func (u *liveUser) Reload(ctx context.Context) error { return u.conn.ReloadContext(ctx) }

func (u *liveUser) Restart(ctx context.Context, units []string) error {
	for _, unit := range units {
		err := waitJob(ctx, unit, func(ch chan<- string) error {
			_, err := u.conn.RestartUnitContext(ctx, unit, "replace", ch)
			return err
		})
		var ended *jobEndedError
		if errors.As(err, &ended) && ended.result == "failed" {
			err = u.awaitRestartRecovery(ctx, unit, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type jobEndedError struct {
	unit   string
	result string
}

func (e *jobEndedError) Error() string {
	return fmt.Sprintf("%s: the job ended %q", e.unit, e.result)
}

func restartedOK(ctx context.Context, conn *sd.Conn, unit string) bool {
	state, err := conn.GetUnitPropertyContext(ctx, unit, "ActiveState")
	if err != nil {
		return false
	}
	sub, err := conn.GetUnitPropertyContext(ctx, unit, "SubState")
	if err != nil {
		return false
	}
	active, _ := state.Value.Value().(string)
	substate, _ := sub.Value.Value().(string)
	return active == string(hostsvc.Active) || (active == string(hostsvc.Activating) && substate != "auto-restart")
}

func (u *liveUser) awaitRestartRecovery(ctx context.Context, unit string, failure error) error {
	ctx, cancel := context.WithTimeout(ctx, restartRecovery)
	defer cancel()
	for {
		if restartedOK(ctx, u.conn, unit) {
			return nil
		}
		select {
		case <-ctx.Done():
			return failure
		case <-time.After(restartPollEvery):
		}
	}
}

func waitJob(ctx context.Context, unit string, start func(chan<- string) error) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, jobTimeout)
		defer cancel()
	}
	ch := make(chan string, 1)
	if err := start(ch); err != nil {
		return fmt.Errorf("%s: %w", unit, err)
	}
	select {
	case r := <-ch:
		if r != "done" {
			return &jobEndedError{unit: unit, result: r}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: the start job did not finish: %w", unit, ctx.Err())
	}
}

func (u *liveUser) EnableNow(ctx context.Context, units []string) error {
	if _, _, err := u.conn.EnableUnitFilesContext(ctx, units, false, false); err != nil {
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

func (u *liveUser) Enable(ctx context.Context, units []string) error {
	_, _, err := u.conn.EnableUnitFilesContext(ctx, units, false, false)
	return err
}

func (u *liveUser) DisableStop(ctx context.Context, units []string) error {
	for _, unit := range units {
		if err := u.Stop(ctx, unit); err != nil {
			return err
		}
	}
	_, err := u.conn.DisableUnitFilesContext(ctx, units, false)
	return err
}

func (u *liveUser) List(ctx context.Context) ([]hostsvc.UnitStatus, error) {
	units, err := u.conn.ListUnitsByPatternsContext(ctx, nil, []string{hostsvc.NamePrefix + "*"})
	if err != nil {
		return nil, err
	}
	out := make([]hostsvc.UnitStatus, 0, len(units))
	for _, s := range units {
		out = append(out, hostsvc.UnitStatus{Name: s.Name, State: hostsvc.ActiveState(s.ActiveState), SubState: s.SubState})
	}
	return out, nil
}

func (u *liveUser) Stop(ctx context.Context, unit string) error {
	err := waitJob(ctx, unit, func(ch chan<- string) error {
		_, err := u.conn.StopUnitContext(ctx, unit, "replace", ch)
		return err
	})
	if err == nil {
		u.conn.ResetFailedUnitContext(ctx, unit)
	}
	return err
}

func (u *liveUser) Close() { u.conn.Close() }

func (s liveSystem) CheckHost(ctx context.Context, a Account, binary string, role hostcontract.Role, stateDir string) error {
	u, err := lookupWithin(ctx.Done(), strconv.Itoa(a.UID), func() (*user.User, error) { return user.LookupId(strconv.Itoa(a.UID)) })
	if err != nil {
		return err
	}
	gids, err := groupIDsWithin(ctx.Done(), u)
	if err != nil {
		return err
	}
	var groups []uint32
	for _, g := range gids {
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return err
		}
		groups = append(groups, uint32(n))
	}
	cmd := exec.CommandContext(ctx, binary, "check-host", "-role", string(role), "-state-dir", stateDir)
	cmd.Dir = stateDir
	cmd.Env = []string{"HOME=" + a.Home, "USER=" + a.Name, "LOGNAME=" + a.Name, "PATH=" + hostPath,
		"XDG_RUNTIME_DIR=" + filepath.Join("/run/user", strconv.Itoa(a.UID))}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(a.UID), Gid: uint32(a.GID), Groups: groups}}
	cmd.Stdout, cmd.Stderr = s.out, s.out
	return cmd.Run()
}

func SelfCheck(ctx context.Context, exe string, role hostcontract.Role, stateDir string) error {
	for _, args := range [][]string{{"version", "--json"}, {"check-host", "-role", string(role), "-state-dir", stateDir}} {
		if b, err := exec.CommandContext(ctx, exe, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s %v: %w: %s", exe, args, err, b)
		}
	}
	return nil
}

func RestartOwnUnits(ctx context.Context, units []string) error {
	um, err := liveSystem{ctx: ctx}.User(ctx, Account{UID: os.Getuid()})
	if err != nil {
		return err
	}
	defer um.Close()
	return um.Restart(ctx, units)
}

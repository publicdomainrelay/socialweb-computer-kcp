package pgunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	Asset         = "postgres"
	SocketName    = ".s.PGSQL.5432"
	MaxSocketPath = 107
	StopTimeout   = 60 * time.Second
	HBA           = "local all all trust\n"
	BackupsDir    = "backups"
	BackupStamp   = "20060102T150405Z"
	ReadyPoll     = 200 * time.Millisecond
)

var (
	ErrOtherMajor = errors.New("pgunit: the data directory was initialised by another postgres major")
	nameRE        = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

type Install struct {
	Root    string
	Bin     string
	Env     []string
	Version string
}

type Server struct {
	Data      string
	SocketDir string
	User      string
	Settings  []string
}

func pin() (versions.Asset, error) {
	all, err := versions.Assets()
	if err != nil {
		return versions.Asset{}, err
	}
	a, ok := all[Asset]
	if !ok {
		return versions.Asset{}, errors.New("pgunit: versions.json pins no postgres asset")
	}
	return a, nil
}

func FromRoot(root string) (Install, error) {
	in, err := At(root)
	if err != nil {
		return Install{}, err
	}
	return in, in.Check()
}

func (i Install) Check() error {
	for _, n := range []string{"postgres", "initdb", "pg_dump"} {
		st, err := os.Stat(i.Path(n))
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("pgunit: %s is not an executable of the postgres asset tree %s", i.Path(n), i.Root)
		}
	}
	return nil
}

func At(root string) (Install, error) {
	if !filepath.IsAbs(root) {
		return Install{}, fmt.Errorf("pgunit: the postgres asset root %q must be absolute (never PATH)", root)
	}
	a, err := pin()
	if err != nil {
		return Install{}, err
	}
	rel, ok := a.Executables["postgres"]
	if !ok {
		return Install{}, errors.New("pgunit: the postgres asset names no postgres executable")
	}
	in := Install{Root: root, Bin: filepath.Join(root, filepath.Dir(filepath.FromSlash(rel))), Version: a.Version}
	if a.LibraryPath != "" {
		in.Env = []string{"LD_LIBRARY_PATH=" + filepath.Join(root, filepath.FromSlash(a.LibraryPath))}
	}
	return in, nil
}

func RootOf(postgresExec string) (string, error) {
	a, err := pin()
	if err != nil {
		return "", err
	}
	rel := filepath.FromSlash(a.Executables["postgres"])
	if rel == "" || !strings.HasSuffix(postgresExec, string(filepath.Separator)+rel) {
		return "", fmt.Errorf("pgunit: %s is not the postgres executable of an extracted asset tree", postgresExec)
	}
	return strings.TrimSuffix(postgresExec, string(filepath.Separator)+rel), nil
}

func (i Install) Path(name string) string { return filepath.Join(i.Bin, name) }

func (i Install) Major() string {
	major, _, _ := strings.Cut(i.Version, ".")
	return major
}

func (i Install) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, i.Path(name), args...)
	cmd.Env = append(os.Environ(), i.Env...)
	return cmd
}

func CheckSocketDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("pgunit: socket directory %q must be absolute", dir)
	}
	if p := filepath.Join(dir, SocketName); len(p) > MaxSocketPath {
		return fmt.Errorf("pgunit: socket path %s is %d bytes, over the %d-byte sun_path limit", p, len(p), MaxSocketPath)
	}
	return nil
}

func CheckData(data, major string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(data, "PG_VERSION"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if got := strings.TrimSpace(string(b)); got != major {
		return true, fmt.Errorf("%w: %s holds major %s, the pinned postgres is %s; adopted, never re-initialised", ErrOtherMajor, data, got, major)
	}
	return true, nil
}

func (i Install) Initdb(ctx context.Context, s Server) (bool, error) {
	if s.User == "" || len(s.User) > 63 || !nameRE.MatchString(s.User) {
		return false, fmt.Errorf("pgunit: user %q: want lower-case letters, digits and underscores", s.User)
	}
	exists, err := CheckData(s.Data, i.Major())
	if err != nil || exists {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(s.Data), 0o700); err != nil {
		return false, err
	}
	out, err := i.command(ctx, "initdb", "-D", s.Data, "-U", s.User, "--auth=trust", "-E", "UTF8",
		"--locale-provider=builtin", "--builtin-locale=C.UTF-8", "--no-instructions").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("pgunit: initdb %s: %w\n%s", s.Data, err, out)
	}
	return true, WriteHBA(s.Data)
}

func WriteHBA(data string) error {
	return os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte(HBA), 0o600)
}

func (i Install) Argv(s Server) ([]string, error) {
	if !filepath.IsAbs(s.Data) {
		return nil, fmt.Errorf("pgunit: data directory %q must be absolute", s.Data)
	}
	if err := CheckSocketDir(s.SocketDir); err != nil {
		return nil, err
	}
	argv := []string{i.Path("postgres"), "-D", s.Data, "-c", "listen_addresses=", "-c", "unix_socket_directories=" + s.SocketDir,
		"-c", "unix_socket_permissions=0700"}
	for _, kv := range s.Settings {
		argv = append(argv, "-c", kv)
	}
	return argv, nil
}

const MemoryMax = 1 << 30

func (i Install) UnitSpec(unit, slice, desc string, s Server) (hostsvc.UnitSpec, error) {
	argv, err := i.Argv(s)
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	return hostsvc.UnitSpec{Name: unit, Description: desc, Exec: argv, Env: i.Env, WorkDir: s.Data, Slice: slice,
		Restart: hostsvc.RestartOnFailure, KillSignal: syscall.SIGINT, StopTimeout: StopTimeout,
		Limits: hostsvc.Limits{MemoryMax: MemoryMax}}, nil
}

func DSN(socketDir, user, db string) string {
	return "postgres://" + user + "@localhost/" + db + "?host=" + socketDir + "&sslmode=disable"
}

func WaitReady(ctx context.Context, socketDir, user string, progress ...func()) error {
	tick := func() {}
	if len(progress) > 0 && progress[0] != nil {
		tick = progress[0]
	}
	var last error
	for {
		tick()
		if last = Ready(ctx, socketDir, user); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pgunit: postgres on %s never answered SELECT 1: %w (last: %v)", socketDir, ctx.Err(), last)
		case <-time.After(ReadyPoll):
		}
	}
}

func Running(data string) bool {
	_, err := os.Stat(filepath.Join(data, "postmaster.pid"))
	return err == nil
}

func (i Install) Dump(ctx context.Context, socketDir, user, db, dir string, at time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(dir, db+"-"+at.UTC().Format(BackupStamp)+".sql")
	msg, err := i.command(ctx, "pg_dump", "-h", socketDir, "-U", user, "-d", db, "-f", out).CombinedOutput()
	if err != nil {
		os.Remove(out)
		return "", fmt.Errorf("pgunit: pg_dump of %s: %w\n%s", db, err, msg)
	}
	if err := os.Chmod(out, 0o600); err != nil {
		return "", err
	}
	st, err := os.Stat(out)
	if err != nil {
		return "", err
	}
	if st.Size() == 0 {
		os.Remove(out)
		return "", fmt.Errorf("pgunit: pg_dump of %s wrote an empty file; refusing a backup that restores nothing", db)
	}
	return out, nil
}

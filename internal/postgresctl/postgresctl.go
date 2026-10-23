package postgresctl

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/logcap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	ChildPrefix = "pg-"
	DefaultUser = "socialweb"
	OutputCap   = 64 << 20
)

var (
	GVR             = v1alpha1.PostgresGVR
	nameRE          = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	DefaultSettings = []string{"wal_level=logical", "autovacuum=on", "track_counts=on"}
)

type Config struct {
	Install    func() (pgunit.Install, error)
	DataRoot   string
	SocketRoot string
	LogDir     string
	Prefix     string
	Slice      string
	User       string
}

type Host struct {
	c Config
}

type InvalidError struct {
	msg string
}

func (e *InvalidError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &InvalidError{msg: fmt.Sprintf(format, args...)}
}

func IsInvalid(err error) bool {
	var e *InvalidError
	return errors.As(err, &e)
}

type Server struct {
	pgunit.Server

	Unit string

	Child v1alpha1.HostProcessSpec

	Initialized bool
}

func New(c Config) *Host {
	c.User = cmp.Or(c.User, DefaultUser)
	return &Host{c: c}
}

func ChildName(name string) string { return ChildPrefix + name }

func (h *Host) User() string { return h.c.User }

func (h *Host) UnitName(name string) string {
	return h.c.Prefix + ChildPrefix + name + ".service"
}

func (h *Host) SocketDir(name string) string {
	return filepath.Join(h.c.SocketRoot, ChildPrefix+name)
}

func (h *Host) logFile(name string) string {
	return filepath.Join(h.c.LogDir, ChildPrefix+name+".log")
}

func (h *Host) DataDir(o v1alpha1.Postgres) string {
	if o.Spec.DataDir != "" {
		return o.Spec.DataDir
	}
	return filepath.Join(h.c.DataRoot, o.Name, "data")
}

func (h *Host) Prepare(ctx context.Context, o v1alpha1.Postgres) (Server, error) {
	if !nameRE.MatchString(o.Name) {
		return Server{}, invalid("name %q: want lower-case letters, digits and dashes, at most 32 (it names a unit and a socket directory)", o.Name)
	}
	if h.c.Install == nil {
		return Server{}, invalid("no postgres asset: this core was given no postgres install")
	}
	in, err := h.c.Install()
	if err != nil {
		return Server{}, invalid("no postgres asset: %s", err.Error())
	}
	if v := o.Spec.Version; v != "" && v != in.Major() && v != in.Version {
		return Server{}, invalid("spec.version %s: this core runs the pinned postgres %s; another version is a new Postgres object (major) or a pin change (minor)", v, in.Version)
	}
	s := Server{Server: pgunit.Server{Data: h.DataDir(o), SocketDir: h.SocketDir(o.Name), User: h.c.User,
		Settings: append(slices.Clone(DefaultSettings), o.Spec.Settings...)}, Unit: h.UnitName(o.Name)}
	if !filepath.IsAbs(s.Data) {
		return Server{}, invalid("dataDir %q must be absolute", s.Data)
	}
	if err := pgunit.CheckSocketDir(s.SocketDir); err != nil {
		return Server{}, invalid("%s", err.Error())
	}
	s.Initialized, err = in.Initdb(ctx, s.Server)
	if err != nil {
		return Server{}, err
	}
	if err := os.MkdirAll(s.SocketDir, 0o700); err != nil {
		return Server{}, err
	}
	s.Child, err = h.childSpec(o, in, s)
	return s, err
}

func (h *Host) childSpec(o v1alpha1.Postgres, in pgunit.Install, s Server) (v1alpha1.HostProcessSpec, error) {
	argv, err := in.Argv(s.Server)
	if err != nil {
		return v1alpha1.HostProcessSpec{}, err
	}
	spec := v1alpha1.HostProcessSpec{Unit: s.Unit, Exec: argv[0], Argv: argv[1:], WorkDir: s.Data,
		RestartPolicy: v1alpha1.HostProcessRestartOnFailure, Slice: h.c.Slice,
		Stop:      &v1alpha1.HostProcessStop{Signal: "SIGINT", GraceSeconds: int32(pgunit.StopTimeout / time.Second)},
		Readiness: &v1alpha1.HostProcessReadiness{PgxSocketDir: s.SocketDir}, Placement: v1alpha1.HostProcessPlacementSupervisor,
		Limits: &v1alpha1.HostProcessLimits{MemoryMax: strconv.FormatUint(pgunit.MemoryMax, 10)}}
	for _, kv := range in.Env {
		k, v, _ := strings.Cut(kv, "=")
		spec.Env = append(spec.Env, v1alpha1.HostProcessEnv{Name: k, Value: v})
	}
	if h.c.LogDir != "" {
		f := h.logFile(o.Name)
		spec.Output = &v1alpha1.HostProcessOutput{Stdout: f, Stderr: f, CapBytes: OutputCap}
	}
	return spec, nil
}

func (s Server) DSN() string { return pgunit.DSN(s.SocketDir, s.User, "postgres") }

func (s Server) Ready(ctx context.Context) error { return pgunit.Ready(ctx, s.SocketDir, s.User) }

func (s Server) Version(ctx context.Context) (string, error) {
	return pgunit.ServerVersion(ctx, s.SocketDir, s.User)
}

func (s Server) Databases(ctx context.Context, names []string) ([]string, error) {
	var created []string
	for _, db := range names {
		made, err := pgunit.CreateDatabase(ctx, s.SocketDir, s.User, db)
		if err != nil {
			return created, fmt.Errorf("creating database %s: %w", db, err)
		}
		if made {
			created = append(created, db)
		}
	}
	return created, nil
}

func (h *Host) Serving(ctx context.Context, o v1alpha1.Postgres) bool {
	return pgunit.Running(h.DataDir(o)) && pgunit.Ready(ctx, h.SocketDir(o.Name), h.c.User) == nil
}

func (h *Host) Remove(o v1alpha1.Postgres) (bool, error) {
	if o.Spec.DeleteData {
		data := h.DataDir(o)
		if err := os.RemoveAll(data); err != nil {
			return false, err
		}
		if o.Spec.DataDir == "" {
			os.Remove(filepath.Dir(data))
		}
		if h.c.LogDir != "" {
			f := h.logFile(o.Name)
			os.Remove(f)
			os.Remove(f + logcap.OldSuffix)
		}
	}
	os.Remove(h.SocketDir(o.Name))
	return o.Spec.DeleteData, nil
}

package kcpdatastore

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	KineStopTimeout = 10 * time.Second
	MaxSocketPath   = 107
	PostgresUser    = "kcp"
	KineDatabase    = "kine"

	SqliteParams = "_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=30000&_txlock=immediate&_stmt_cache_size=20&_auto_vacuum=incremental"
)

var PostgresSettings = []string{"autovacuum=on", "track_counts=on"}

type KineConfig struct {
	Unit              string
	Slice             string
	Exec              string
	Dir               string
	Socket            string
	PostgresSocketDir string
}

type PostgresConfig struct {
	Unit      string
	Slice     string
	Install   pgunit.Install
	Data      string
	SocketDir string
}

func (c PostgresConfig) Server() pgunit.Server {
	return pgunit.Server{Data: c.Data, SocketDir: c.SocketDir, User: PostgresUser, Settings: PostgresSettings}
}

func checkSocket(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("kcpdatastore: socket path %q must be absolute", path)
	}
	if len(path) > MaxSocketPath {
		return fmt.Errorf("kcpdatastore: socket path %s is %d bytes, over the %d-byte sun_path limit", path, len(path), MaxSocketPath)
	}
	return nil
}

func kineArgv(c KineConfig, endpoint string) ([]string, error) {
	if !filepath.IsAbs(c.Exec) {
		return nil, fmt.Errorf("kcpdatastore: the kine executable must be an absolute path, got %q (dev builds take it from kine-path, never PATH)", c.Exec)
	}
	if !filepath.IsAbs(c.Dir) {
		return nil, errors.New("kcpdatastore: the kine directory must be absolute")
	}
	if err := checkSocket(c.Socket); err != nil {
		return nil, err
	}
	return []string{c.Exec,
		"--endpoint", endpoint,
		"--listen-address", "unix://" + c.Socket,
		"--metrics-bind-address=0",
	}, nil
}

func KineSqliteArgv(c KineConfig) ([]string, error) {
	return kineArgv(c, "sqlite://"+filepath.Join(c.Dir, SqliteFile)+"?"+SqliteParams)
}

func PostgresDSN(socketDir string) string {
	return pgunit.DSN(socketDir, PostgresUser, KineDatabase)
}

func KinePostgresArgv(c KineConfig) ([]string, error) {
	if err := pgunit.CheckSocketDir(c.PostgresSocketDir); err != nil {
		return nil, err
	}
	return kineArgv(c, PostgresDSN(c.PostgresSocketDir))
}

const KineMemoryMax = 512 << 20

func kineSpec(c KineConfig, argv []string, desc string) hostsvc.UnitSpec {
	return hostsvc.UnitSpec{
		Name:        c.Unit,
		Description: desc,
		Exec:        argv,
		WorkDir:     c.Dir,
		Slice:       c.Slice,
		Restart:     hostsvc.RestartOnFailure,
		KillSignal:  syscall.SIGTERM,
		StopTimeout: KineStopTimeout,
		Limits:      hostsvc.Limits{MemoryMax: KineMemoryMax},
	}
}

func KineSqliteSpec(c KineConfig) (hostsvc.UnitSpec, error) {
	argv, err := KineSqliteArgv(c)
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	return kineSpec(c, argv, "kine on sqlite for one application instance's kcp"), nil
}

func KinePostgresSpec(c KineConfig, store string) (hostsvc.UnitSpec, error) {
	argv, err := KinePostgresArgv(c)
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	spec := kineSpec(c, argv, "kine on the application instance's postgres for one kcp slot")
	spec.After, spec.Wants = []string{store}, []string{store}
	return spec, nil
}

func PostgresSpec(c PostgresConfig) (hostsvc.UnitSpec, error) {
	if c.Install.Bin == "" || !filepath.IsAbs(c.Install.Bin) {
		return hostsvc.UnitSpec{}, fmt.Errorf("kcpdatastore: the postgres asset tree is unset or relative (%q)", c.Install.Root)
	}
	return c.Install.UnitSpec(c.Unit, c.Slice, "postgres holding one application instance's kcp store (unix socket only)", c.Server())
}

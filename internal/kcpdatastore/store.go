package kcpdatastore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Role string

type Kind string

const (
	RoleGateway      Role = "gateway"
	RoleBidder       Role = "bidder"
	KindKineSqlite   Kind = "kine-sqlite"
	KindKinePostgres Kind = "kine-postgres"
)

const (
	SqliteFile      = "kine.db"
	BackupsDir      = "backups"
	BackupTimestamp = "20060102T150405Z"
)

var SqliteCompanions = []string{"-wal", "-shm"}

type Assets struct {
	Kcp  string
	Kine string
}

func StoreFor(role Role, profile string) (Kind, error) {
	switch profile {
	case "", "quick", "market-network", "prodlike":
	default:
		return "", fmt.Errorf("kcpdatastore: unknown profile %q", profile)
	}
	switch role {
	case RoleBidder:
		return KindKineSqlite, nil
	case RoleGateway:
		if profile == "quick" {
			return KindKineSqlite, nil
		}
		return KindKinePostgres, nil
	}
	return "", fmt.Errorf("kcpdatastore: unknown role %q", role)
}

func NeedsBackup(from, to Assets) bool {
	return from.Kcp != to.Kcp || from.Kine != to.Kine
}

func copyChecked(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		out.Close()
		return n, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return n, err
	}
	return n, out.Close()
}

func BackupSqlite(dir string, at time.Time) (string, error) {
	db := filepath.Join(dir, SqliteFile)
	fi, err := os.Stat(db)
	if err != nil {
		return "", fmt.Errorf("kcpdatastore: no sqlite store to copy: %w", err)
	}
	if fi.Size() == 0 {
		return "", fmt.Errorf("kcpdatastore: %s is empty; refusing a backup that restores nothing", db)
	}
	out := filepath.Join(dir, BackupsDir, at.UTC().Format(BackupTimestamp))
	if err := os.MkdirAll(out, 0o700); err != nil {
		return "", err
	}
	n, err := copyChecked(db, filepath.Join(out, SqliteFile))
	if err != nil {
		return "", fmt.Errorf("kcpdatastore: copying %s: %w", db, err)
	}
	if n != fi.Size() || n == 0 {
		return "", fmt.Errorf("kcpdatastore: copied %d of %d bytes of %s", n, fi.Size(), db)
	}
	for _, suffix := range SqliteCompanions {
		src := db + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := copyChecked(src, filepath.Join(out, SqliteFile+suffix)); err != nil {
			return "", fmt.Errorf("kcpdatastore: copying %s: %w", src, err)
		}
	}
	return out, nil
}

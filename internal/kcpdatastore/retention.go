package kcpdatastore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultBackupKeep     = 5
	DefaultBackupKeepDays = 14
)

type BackupRetention struct {
	Keep   int
	MaxAge time.Duration
}

func DefaultRetention() BackupRetention {
	return BackupRetention{Keep: DefaultBackupKeep, MaxAge: DefaultBackupKeepDays * 24 * time.Hour}
}

func RetentionOf(keep, keepDays int) (BackupRetention, error) {
	if keep < 1 {
		return BackupRetention{}, fmt.Errorf("kcpdatastore: store-backup-keep must be 1 or more, got %d", keep)
	}
	if keepDays < 1 {
		return BackupRetention{}, fmt.Errorf("kcpdatastore: store-backup-keep-days must be 1 or more, got %d", keepDays)
	}
	return BackupRetention{Keep: keep, MaxAge: time.Duration(keepDays) * 24 * time.Hour}, nil
}

type backupEntry struct {
	name string
	at   time.Time
}

func PruneBackups(dir, prefix, suffix, newest string, r BackupRetention, now time.Time) ([]string, error) {
	if r.Keep < 1 {
		r = DefaultRetention()
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var found []backupEntry
	for _, de := range des {
		mid, ok := strings.CutPrefix(de.Name(), prefix)
		if !ok {
			continue
		}
		if mid, ok = strings.CutSuffix(mid, suffix); !ok {
			continue
		}
		at, err := time.Parse(BackupTimestamp, mid)
		if err != nil {
			continue
		}
		found = append(found, backupEntry{name: de.Name(), at: at})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].at.After(found[j].at) })
	keepName := filepath.Base(newest)
	var removed []string
	var errs []error
	for i, e := range found {
		if e.name == keepName {
			continue
		}
		if i < r.Keep && (r.MaxAge <= 0 || now.Sub(e.at) <= r.MaxAge) {
			continue
		}
		p := filepath.Join(dir, e.name)
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, p)
	}
	return removed, errors.Join(errs...)
}

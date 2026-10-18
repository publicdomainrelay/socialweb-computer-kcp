package storemaint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var sqliteMagic = []byte("SQLite format 3\x00")

var errNotDenoKv = errors.New("not a deno kv database")

type DenoKvConfig struct {
	Dirs     []string
	Interval time.Duration
	Log      *slog.Logger
}

func KeepDenoKv(ctx context.Context, c DenoKvConfig) {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	log := slog.New(slog.DiscardHandler)
	if c.Log != nil {
		log = c.Log.With("component", "storemaint", "store", "denokv")
	}
	wait := DefaultFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = c.Interval
		for _, path := range denoKvFiles(c.Dirs) {
			res, err := MaintainDenoKv(ctx, path)
			if errors.Is(err, errNotDenoKv) || errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				log.Warn("maintaining a deno kv database", "path", path, "err", err.Error())
				continue
			}
			if res.Converted || res.PagesFreed > 0 {
				log.Info("deno kv database maintained", "path", path, "converted", res.Converted, "rows", res.After.Rows,
					"fileBytesBefore", res.Before.FileBytes, "fileBytesAfter", res.After.FileBytes, "walBytesAfter", res.After.WalBytes,
					"freePagesBefore", res.Before.FreePages, "freePagesAfter", res.After.FreePages, "pagesFreed", res.PagesFreed,
					"checkpointed", res.Checkpointed)
			}
		}
	}
}

func MaintainDenoKv(ctx context.Context, path string) (SqliteResult, error) {
	db, err := openSqlite(path)
	if err != nil {
		return SqliteResult{}, err
	}
	defer db.Close()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('kv', 'data_version')").Scan(&tables); err != nil || tables != 2 {
		return SqliteResult{}, errNotDenoKv
	}
	return maintain(ctx, db, path, "kv", true)
}

func denoKvFiles(dirs []string) []string {
	var out []string
	head := make([]byte, len(sqliteMagic))
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			if strings.HasSuffix(p, "-wal") || strings.HasSuffix(p, "-shm") || strings.HasSuffix(p, "-journal") {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			if _, err := io.ReadFull(f, head); err == nil && bytes.Equal(head, sqliteMagic) {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

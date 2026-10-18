package storemaint

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

const (
	AutoVacuumNone        = "none"
	AutoVacuumFull        = "full"
	AutoVacuumIncremental = "incremental"

	VacuumChunkPages = 2000
	BusyTimeoutMs    = 30000
)

type SqliteReading struct {
	AutoVacuum string
	PageSize   int64
	PageCount  int64
	FreePages  int64
	Rows       int64
	FileBytes  int64
	WalBytes   int64
}

func (r SqliteReading) FreeBytes() int64 { return r.FreePages * r.PageSize }

type SqliteResult struct {
	Before, After SqliteReading
	PagesFreed    int64
	Checkpointed  bool
	Converted     bool
}

func autoVacuumName(mode int64) string {
	switch mode {
	case 1:
		return AutoVacuumFull
	case 2:
		return AutoVacuumIncremental
	}
	return AutoVacuumNone
}

func openSqlite(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("mode", "rw")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeoutMs))
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func pragmaInt(ctx context.Context, db *sql.DB, name string) (int64, error) {
	var v int64
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		return 0, fmt.Errorf("storemaint: PRAGMA %s: %w", name, err)
	}
	return v, nil
}

func readSqlite(ctx context.Context, db *sql.DB, path, table string) (SqliteReading, error) {
	var r SqliteReading
	mode, err := pragmaInt(ctx, db, "auto_vacuum")
	if err != nil {
		return r, err
	}
	r.AutoVacuum = autoVacuumName(mode)
	if r.PageSize, err = pragmaInt(ctx, db, "page_size"); err != nil {
		return r, err
	}
	if r.PageCount, err = pragmaInt(ctx, db, "page_count"); err != nil {
		return r, err
	}
	if r.FreePages, err = pragmaInt(ctx, db, "freelist_count"); err != nil {
		return r, err
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&r.Rows); err != nil {
		return r, fmt.Errorf("storemaint: counting the %s rows of %s: %w", table, path, err)
	}
	if fi, err := os.Stat(path); err == nil {
		r.FileBytes = fi.Size()
	}
	if fi, err := os.Stat(path + "-wal"); err == nil {
		r.WalBytes = fi.Size()
	}
	return r, nil
}

func ReadSqlite(ctx context.Context, path string) (SqliteReading, error) {
	db, err := openSqlite(path)
	if err != nil {
		return SqliteReading{}, err
	}
	defer db.Close()
	return readSqlite(ctx, db, path, "kine")
}

func MaintainSqlite(ctx context.Context, path string) (SqliteResult, error) {
	db, err := openSqlite(path)
	if err != nil {
		return SqliteResult{}, err
	}
	defer db.Close()
	return maintain(ctx, db, path, "kine", false)
}

func maintain(ctx context.Context, db *sql.DB, path, table string, convert bool) (SqliteResult, error) {
	var res SqliteResult
	var err error
	if res.Before, err = readSqlite(ctx, db, path, table); err != nil {
		return res, err
	}
	res.After = res.Before
	if res.Before.AutoVacuum != AutoVacuumIncremental {
		if !convert {
			return res, nil
		}
		if _, err := db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return res, fmt.Errorf("storemaint: setting auto_vacuum on %s: %w", path, err)
		}
		if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
			return res, fmt.Errorf("storemaint: converting %s to incremental auto_vacuum: %w", path, err)
		}
		res.Converted = true
	}
	for left := res.Before.FreePages; left > 0; {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", VacuumChunkPages)); err != nil {
			return res, fmt.Errorf("storemaint: incremental_vacuum on %s: %w", path, err)
		}
		now, err := pragmaInt(ctx, db, "freelist_count")
		if err != nil {
			return res, err
		}
		if now >= left {
			break
		}
		left = now
	}
	var busy, logFrames, checkpointed int64
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err == nil {
		res.Checkpointed = busy == 0
	}
	if res.After, err = readSqlite(ctx, db, path, table); err != nil {
		return res, err
	}
	if res.Converted {
		res.PagesFreed = res.Before.PageCount - res.After.PageCount
		return res, nil
	}
	res.PagesFreed = res.Before.FreePages - res.After.FreePages
	return res, nil
}

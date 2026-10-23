package storemaint

import (
	"context"
	"fmt"
	"strconv"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpdatastore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	KineTable = "kine"

	KineTableAutovacuum = "ALTER TABLE kine SET (autovacuum_enabled = true, autovacuum_vacuum_threshold = 1000, " +
		"autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_threshold = 1000, autovacuum_analyze_scale_factor = 0.05)"

	kineStatsSQL = "SELECT pg_total_relation_size('kine'), COALESCE(s.n_live_tup, 0), COALESCE(s.n_dead_tup, 0), " +
		"COALESCE(s.autovacuum_count, 0), COALESCE(to_char(s.last_autovacuum AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), '') " +
		"FROM pg_class c LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid WHERE c.oid = 'kine'::regclass"
)

type PostgresReading struct {
	Autovacuum      string
	TotalBytes      int64
	LiveTuples      int64
	DeadTuples      int64
	AutovacuumCount int64
	LastAutovacuum  string
}

func ApplyKineAutovacuum(ctx context.Context, socketDir string) error {
	_, err := pgunit.Query(ctx, socketDir, kcpdatastore.PostgresUser, kcpdatastore.KineDatabase, KineTableAutovacuum)
	return err
}

func ReadPostgres(ctx context.Context, socketDir string) (PostgresReading, error) {
	var r PostgresReading
	rows, err := pgunit.Query(ctx, socketDir, kcpdatastore.PostgresUser, kcpdatastore.KineDatabase, "SHOW autovacuum")
	if err != nil {
		return r, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return r, fmt.Errorf("storemaint: SHOW autovacuum answered %v", rows)
	}
	r.Autovacuum = rows[0][0]
	rows, err = pgunit.Query(ctx, socketDir, kcpdatastore.PostgresUser, kcpdatastore.KineDatabase, kineStatsSQL)
	if err != nil {
		return r, err
	}
	if len(rows) != 1 || len(rows[0]) != 5 {
		return r, fmt.Errorf("storemaint: the kine table statistics answered %v", rows)
	}
	row := rows[0]
	for i, dst := range []*int64{&r.TotalBytes, &r.LiveTuples, &r.DeadTuples, &r.AutovacuumCount} {
		if *dst, err = strconv.ParseInt(row[i], 10, 64); err != nil {
			return r, fmt.Errorf("storemaint: the kine table statistic %d is %q: %w", i, row[i], err)
		}
	}
	r.LastAutovacuum = row[4]
	return r, nil
}

package storemaint

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpdatastore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultInterval = 5 * time.Minute
	DefaultFirst    = 30 * time.Second
)

type Config struct {
	Kind        kcpdatastore.Kind
	SqliteDirs  []string
	PgSocketDir string
	Interval    time.Duration
	First       time.Duration
	Log         *slog.Logger
}

type Monitor struct {
	cfg          Config
	usage        atomic.Pointer[v1alpha1.StoreUsage]
	vacuums      int64
	reclaimed    int64
	lastVacuumAt string
	tableTuned   bool
}

func New(cfg Config) *Monitor {
	cfg.Interval = opt.Positive(cfg.Interval, DefaultInterval)
	cfg.First = opt.Positive(cfg.First, DefaultFirst)
	return &Monitor{cfg: cfg}
}

func (m *Monitor) Usage() *v1alpha1.StoreUsage { return m.usage.Load() }

func (m *Monitor) log() *slog.Logger {
	if m.cfg.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.cfg.Log.With("component", "storemaint", "store", string(m.cfg.Kind))
}

func (m *Monitor) Run(ctx context.Context) {
	wait := m.cfg.First
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		m.Pass(ctx)
		wait = m.cfg.Interval
	}
}

func (m *Monitor) Pass(ctx context.Context) {
	switch m.cfg.Kind {
	case kcpdatastore.KindKineSqlite:
		m.passSqlite(ctx)
	case kcpdatastore.KindKinePostgres:
		m.passPostgres(ctx)
	}
}

func (m *Monitor) passSqlite(ctx context.Context) {
	for _, dir := range m.cfg.SqliteDirs {
		path := filepath.Join(dir, kcpdatastore.SqliteFile)
		res, err := MaintainSqlite(ctx, path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			m.log().Warn("maintaining the sqlite store", "path", path, "err", err.Error())
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if res.PagesFreed > 0 {
			m.vacuums++
			m.reclaimed += res.PagesFreed * res.After.PageSize
			m.lastVacuumAt = now
		}
		m.log().Info("sqlite store maintained", "path", path, "autoVacuum", res.Before.AutoVacuum,
			"rows", res.After.Rows, "fileBytesBefore", res.Before.FileBytes, "fileBytesAfter", res.After.FileBytes,
			"walBytesAfter", res.After.WalBytes, "freePagesBefore", res.Before.FreePages, "freePagesAfter", res.After.FreePages,
			"pagesFreed", res.PagesFreed, "checkpointed", res.Checkpointed)
		m.usage.Store(&v1alpha1.StoreUsage{Kind: string(kcpdatastore.KindKineSqlite), Path: path, Bytes: res.After.FileBytes,
			FreeBytes: res.After.FreeBytes(), Rows: res.After.Rows, AutoVacuum: res.After.AutoVacuum, Vacuums: m.vacuums,
			ReclaimedBytes: m.reclaimed, LastVacuumAt: m.lastVacuumAt, SampledAt: now})
		return
	}
}

func (m *Monitor) passPostgres(ctx context.Context) {
	if !m.tableTuned {
		if err := ApplyKineAutovacuum(ctx, m.cfg.PgSocketDir); err != nil {
			m.log().Warn("setting the kine table's autovacuum parameters; kine has not made the table yet, or postgres is down", "err", err.Error())
			return
		}
		m.tableTuned = true
		m.log().Info("the kine table's autovacuum parameters are set")
	}
	r, err := ReadPostgres(ctx, m.cfg.PgSocketDir)
	if err != nil {
		m.log().Warn("reading the kine table statistics", "err", err.Error())
		return
	}
	m.log().Info("postgres kine table read", "autovacuum", r.Autovacuum, "totalBytes", r.TotalBytes, "liveTuples", r.LiveTuples,
		"deadTuples", r.DeadTuples, "autovacuums", r.AutovacuumCount, "lastAutovacuum", r.LastAutovacuum)
	m.usage.Store(&v1alpha1.StoreUsage{Kind: string(kcpdatastore.KindKinePostgres), Path: KineTable, Bytes: r.TotalBytes, Rows: r.LiveTuples,
		DeadTuples: r.DeadTuples, AutoVacuum: r.Autovacuum, Vacuums: r.AutovacuumCount, LastVacuumAt: r.LastAutovacuum,
		SampledAt: time.Now().UTC().Format(time.RFC3339)})
}

package plc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Store interface {
	GetCurrentOps(ctx context.Context, did string) ([]LogEntry, error)
	GetAuditLog(ctx context.Context, did string) ([]LogEntry, error)
	GetOpByCID(ctx context.Context, did string, cid string) (*LogEntry, error)
	InsertOp(ctx context.Context, entry LogEntry) error
	NullifyOps(ctx context.Context, did string, cids []string) error
	ExportLogs(ctx context.Context, after *time.Time, count int) ([]LogEntry, error)
}

type MemoryStore struct {
	mu   sync.RWMutex
	logs map[string][]LogEntry
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{logs: map[string][]LogEntry{}}
}

func (s *MemoryStore) GetCurrentOps(ctx context.Context, did string) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []LogEntry{}
	for _, e := range s.logs[did] {
		if !e.Nullified {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *MemoryStore) GetAuditLog(ctx context.Context, did string) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]LogEntry{}, s.logs[did]...)
	return out, nil
}

func (s *MemoryStore) GetOpByCID(ctx context.Context, did string, cid string) (*LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.logs[did] {
		if s.logs[did][i].CID == cid {
			e := s.logs[did][i]
			return &e, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) InsertOp(ctx context.Context, entry LogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[entry.DID] = append(s.logs[entry.DID], entry)
	return nil
}

func (s *MemoryStore) NullifyOps(ctx context.Context, did string, cids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]struct{}, len(cids))
	for _, c := range cids {
		set[c] = struct{}{}
	}
	for i := range s.logs[did] {
		if _, ok := set[s.logs[did][i].CID]; ok {
			s.logs[did][i].Nullified = true
		}
	}
	return nil
}

func (s *MemoryStore) ExportLogs(ctx context.Context, after *time.Time, count int) ([]LogEntry, error) {
	s.mu.RLock()
	all := []LogEntry{}
	for _, entries := range s.logs {
		all = append(all, entries...)
	}
	s.mu.RUnlock()
	if after != nil {
		filtered := all[:0]
		for _, e := range all {
			t, err := time.Parse(time.RFC3339Nano, e.CreatedAt)
			if err != nil || t.After(*after) {
				filtered = append(filtered, e)
			}
		}
		all = filtered
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt < all[j].CreatedAt })
	if count >= 0 && len(all) > count {
		all = all[:count]
	}
	return all, nil
}

func (s *MemoryStore) snapshot() map[string][]LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]LogEntry, len(s.logs))
	for did, entries := range s.logs {
		out[did] = append([]LogEntry{}, entries...)
	}
	return out
}

func (s *MemoryStore) replace(logs map[string][]LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = logs
}

type FileStore struct {
	*MemoryStore
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) (*FileStore, error) {
	fs := &FileStore{MemoryStore: NewMemoryStore(), path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fs, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return fs, nil
	}
	var logs map[string][]LogEntry
	if err := json.Unmarshal(b, &logs); err != nil {
		return nil, err
	}
	if logs == nil {
		logs = map[string][]LogEntry{}
	}
	fs.MemoryStore.replace(logs)
	return fs, nil
}

func (s *FileStore) InsertOp(ctx context.Context, entry LogEntry) error {
	if err := s.MemoryStore.InsertOp(ctx, entry); err != nil {
		return err
	}
	return s.save()
}

func (s *FileStore) NullifyOps(ctx context.Context, did string, cids []string) error {
	if err := s.MemoryStore.NullifyOps(ctx, did, cids); err != nil {
		return err
	}
	return s.save()
}

func (s *FileStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.MemoryStore.snapshot())
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

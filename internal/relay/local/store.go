package local

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	atrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/relay"
)

type diskState struct {
	Seq         int64                 `json:"seq,omitempty"`
	Hosts       []atrelay.HostInfo    `json:"hosts"`
	Accounts    []atrelay.AccountInfo `json:"accounts"`
	Collections map[string][]string   `json:"collections"`
}

type fileStore struct {
	*atrelay.MemoryStore
	path        string
	mu          sync.Mutex
	collections map[string]map[string]struct{}
	dirty       atomic.Bool
	writeMu     sync.Mutex
	changed     func()
	seq         int64
	head        func() int64
}

var _ atrelay.Store = (*fileStore)(nil)

func openFileStore(path string) (*fileStore, error) {
	store := &fileStore{
		MemoryStore: atrelay.NewMemoryStore(),
		path:        path,
		collections: map[string]map[string]struct{}{},
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, err
	}
	if len(body) == 0 {
		return store, nil
	}
	var state diskState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, err
	}
	store.seq = state.Seq
	for _, host := range state.Hosts {
		store.MemoryStore.PutHost(host)
	}
	for _, account := range state.Accounts {
		store.MemoryStore.PutAccount(account)
	}
	for collection, dids := range state.Collections {
		set := map[string]struct{}{}
		for _, did := range dids {
			set[did] = struct{}{}
			store.MemoryStore.AddCollection(collection, did)
		}
		store.collections[collection] = set
	}
	return store, nil
}

func (s *fileStore) touch() {
	s.dirty.Store(true)
	if s.changed != nil {
		s.changed()
	}
}

func (s *fileStore) PutHost(info atrelay.HostInfo) {
	s.MemoryStore.PutHost(info)
	s.touch()
}

func (s *fileStore) PutAccount(info atrelay.AccountInfo) {
	s.MemoryStore.PutAccount(info)
	s.touch()
}

func (s *fileStore) AddCollection(collection, did string) {
	s.MemoryStore.AddCollection(collection, did)
	s.mu.Lock()
	set := s.collections[collection]
	if set == nil {
		set = map[string]struct{}{}
		s.collections[collection] = set
	}
	set[did] = struct{}{}
	s.mu.Unlock()
	s.touch()
}

func (s *fileStore) flush() error {
	if !s.dirty.Swap(false) {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	state := diskState{
		Seq:      s.seq,
		Hosts:    s.ListHosts(),
		Accounts: s.ListAccounts(),
	}
	if s.head != nil {
		state.Seq = max(state.Seq, s.head())
	}
	s.mu.Lock()
	if len(s.collections) > 0 {
		state.Collections = make(map[string][]string, len(s.collections))
		for collection, set := range s.collections {
			dids := make([]string, 0, len(set))
			for did := range set {
				dids = append(dids, did)
			}
			sort.Strings(dids)
			state.Collections[collection] = dids
		}
	}
	s.mu.Unlock()
	body, err := json.Marshal(state)
	if err != nil {
		s.dirty.Store(true)
		return err
	}
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			s.dirty.Store(true)
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		s.dirty.Store(true)
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		s.dirty.Store(true)
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		s.dirty.Store(true)
		return err
	}
	if err := tmp.Close(); err != nil {
		s.dirty.Store(true)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		s.dirty.Store(true)
		return err
	}
	return nil
}

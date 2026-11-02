package files

import (
	"context"
	"fmt"
	"sync"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
)

type MemoryRepoKeyStore struct {
	mu   sync.Mutex
	keys map[syntax.DID]atcrypto.PrivateKeyExportable
}

var _ atpds.RepoKeyStore = (*MemoryRepoKeyStore)(nil)

func NewMemoryRepoKeyStore() *MemoryRepoKeyStore {
	return &MemoryRepoKeyStore{keys: map[syntax.DID]atcrypto.PrivateKeyExportable{}}
}

func (s *MemoryRepoKeyStore) PutRepoKey(_ context.Context, did syntax.DID, key atcrypto.PrivateKeyExportable) error {
	if key == nil {
		return fmt.Errorf("pds: no repo signing key for %s", did)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[did] = key
	return nil
}

func (s *MemoryRepoKeyStore) GetRepoKey(_ context.Context, did syntax.DID) (atcrypto.PrivateKeyExportable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[did]
	if !ok {
		return nil, fmt.Errorf("pds: no repo signing key for %s", did)
	}
	return key, nil
}

func (s *MemoryRepoKeyStore) DeleteRepoKey(_ context.Context, did syntax.DID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, did)
	return nil
}

func (s *MemoryRepoKeyStore) DIDs() []syntax.DID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]syntax.DID, 0, len(s.keys))
	for did := range s.keys {
		out = append(out, did)
	}
	return out
}

package vaultmemory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

var ErrNotFound = fmt.Errorf("memory vault: secret not found: %w", openbao.ErrNotFound)

var ErrInvalidRef = errors.New("memory vault: a KV call needs a mount and a path")

type kvKey struct {
	mount string
	path  string
}

type entry struct {
	data    map[string]any
	version int
}

type Store struct {
	mu       sync.RWMutex
	entries  map[kvKey]entry
	onChange func()
}

func (s *Store) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

var _ contractvault.KV = (*Store)(nil)

func NewStore() *Store { return &Store{entries: map[kvKey]entry{}} }

func (s *Store) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k, err := kvKeyOf(mount, path)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	e, ok := s.entries[k]
	s.mu.RUnlock()
	if !ok || (version > 0 && version != e.version) {
		return nil, fmt.Errorf("memory vault: %s/%s: %w", k.mount, k.path, ErrNotFound)
	}
	return cloneData(e.data), nil
}

func (s *Store) Write(ctx context.Context, mount, path string, data map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := kvKeyOf(mount, path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.entries == nil {
		s.entries = map[kvKey]entry{}
	}
	s.entries[k] = entry{data: cloneData(data), version: s.entries[k].version + 1}
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, mount, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := kvKeyOf(mount, path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.entries, k)
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

func (s *Store) List(ctx context.Context, mount, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" {
		return nil, ErrInvalidRef
	}
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]struct{}{}
	s.mu.RLock()
	for k := range s.entries {
		if k.mount != mount || !strings.HasPrefix(k.path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k.path, prefix)
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i+1]
		}
		seen[rest] = struct{}{}
	}
	s.mu.RUnlock()
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

type StoreEntry struct {
	Mount   string         `json:"mount"`
	Path    string         `json:"path"`
	Version int            `json:"version"`
	Data    map[string]any `json:"data"`
}

type StoreSnapshot struct {
	Entries []StoreEntry `json:"entries"`
}

func (s *Store) Snapshot() StoreSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := StoreSnapshot{Entries: make([]StoreEntry, 0, len(s.entries))}
	for k, e := range s.entries {
		out.Entries = append(out.Entries, StoreEntry{
			Mount: k.mount, Path: k.path, Version: e.version, Data: cloneData(e.data),
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Mount != out.Entries[j].Mount {
			return out.Entries[i].Mount < out.Entries[j].Mount
		}
		return out.Entries[i].Path < out.Entries[j].Path
	})
	return out
}

func (s *Store) Restore(snapshot StoreSnapshot) {
	entries := make(map[kvKey]entry, len(snapshot.Entries))
	for _, e := range snapshot.Entries {
		k, err := kvKeyOf(e.Mount, e.Path)
		if err != nil {
			continue
		}
		version := e.Version
		if version < 1 {
			version = 1
		}
		entries[k] = entry{data: cloneData(e.Data), version: version}
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
}

type Secrets struct {
	store *Store
}

var _ atproto.SecretStore = (*Secrets)(nil)

func NewSecrets(store *Store) *Secrets {
	if store == nil {
		store = NewStore()
	}
	return &Secrets{store: store}
}

func (s *Secrets) Get(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error) {
	data, err := s.store.Read(ctx, ref.Mount, ref.Path, 0)
	if errors.Is(err, ErrNotFound) {
		return nil, atproto.ErrSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, atproto.ErrSecretNotFound
	}
	return data, nil
}

func (s *Secrets) Put(ctx context.Context, ref commonv1.SecretRef, data map[string]any) error {
	return s.store.Write(ctx, ref.Mount, ref.Path, data)
}

func (s *Secrets) Delete(ctx context.Context, ref commonv1.SecretRef) error {
	return s.store.Delete(ctx, ref.Mount, ref.Path)
}

type Credentials struct {
	store *Store
}

var (
	_ component.CredentialResolver = (*Credentials)(nil)
	_ component.CredentialWriter   = (*Credentials)(nil)
	_ component.CredentialDeleter  = (*Credentials)(nil)
)

func NewCredentials(store *Store) *Credentials {
	if store == nil {
		store = NewStore()
	}
	return &Credentials{store: store}
}

func (c *Credentials) Resolve(ctx context.Context, ref component.CredentialRef) (component.Credential, error) {
	if err := ref.Check(); err != nil {
		return component.Credential{}, err
	}
	data, err := c.store.Read(ctx, ref.Mount, ref.Path, ref.Version)
	if err != nil {
		return component.Credential{}, fmt.Errorf("memory vault: reading %s/%s: %w", ref.Mount, ref.Path, err)
	}
	if len(data) == 0 {
		return component.Credential{}, fmt.Errorf("memory vault: %s/%s: %w", ref.Mount, ref.Path, ErrNotFound)
	}
	return component.Credential{Ref: ref, Data: filterKeys(data, ref.Keys)}, nil
}

func (c *Credentials) WriteCredential(ctx context.Context, ref component.CredentialRef, data map[string]any) error {
	if err := ref.Check(); err != nil {
		return err
	}
	if data == nil {
		return errors.New("memory vault: a credential write needs data")
	}
	return c.store.Write(ctx, ref.Mount, ref.Path, data)
}

func (c *Credentials) DeleteCredential(ctx context.Context, ref component.CredentialRef) error {
	if err := ref.Check(); err != nil {
		return err
	}
	return c.store.Delete(ctx, ref.Mount, ref.Path)
}

func kvKeyOf(mount, path string) (kvKey, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimPrefix(strings.TrimPrefix(path, mount+"/data/"), mount+"/metadata/")
	if mount == "" || path == "" {
		return kvKey{}, ErrInvalidRef
	}
	return kvKey{mount: mount, path: path}, nil
}

func filterKeys(data map[string]any, keys []string) map[string]any {
	if len(keys) == 0 {
		return data
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := data[key]; ok {
			out[key] = value
		}
	}
	return out
}

func cloneData(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return append([]byte(nil), x...)
	case map[string]any:
		return cloneData(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}

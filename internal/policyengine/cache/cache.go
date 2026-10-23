package cache

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

const (
	RecordValueFile  = "value.json"
	PolicyResultFile = "result.json"
	PolicyMetaFile   = "meta.json"
)

type Store struct {
	mu         sync.Mutex
	entries    map[string]models.CacheEntry
	order      []string
	maxEntries int
	maxBytes   int64
	bytes      int64
}

func NewStore(maxEntries int, maxBytes int64) *Store {
	return &Store{
		entries:    make(map[string]models.CacheEntry),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

func (s *Store) Seed(value models.Cache) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range value {
		s.setLocked(key, entry)
	}
}

func (s *Store) Snapshot() models.Cache {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(models.Cache, len(s.entries))
	for key, entry := range s.entries {
		copied := make(models.CacheEntry, len(entry))
		maps.Copy(copied, entry)
		out[key] = copied
	}
	return out
}

func (s *Store) Merge(value models.Cache) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range value {
		s.setLocked(key, entry)
	}
}

func (s *Store) Get(key string) (models.CacheEntry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	s.touchLocked(key)
	copied := make(models.CacheEntry, len(entry))
	maps.Copy(copied, entry)
	return copied, true
}

func (s *Store) Set(key string, entry models.CacheEntry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(key, entry)
}

func (s *Store) Delete(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(key)
}

func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Store) setLocked(key string, entry models.CacheEntry) {
	if _, exists := s.entries[key]; exists {
		s.deleteLocked(key)
	}
	s.entries[key] = entry
	s.order = append(s.order, key)
	s.bytes += entryBytes(key, entry)
	s.evictLocked()
}

func (s *Store) deleteLocked(key string) {
	entry, ok := s.entries[key]
	if !ok {
		return
	}
	s.bytes -= entryBytes(key, entry)
	delete(s.entries, key)
	for i, existing := range s.order {
		if existing == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *Store) touchLocked(key string) {
	for i, existing := range s.order {
		if existing == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			s.order = append(s.order, key)
			return
		}
	}
}

func (s *Store) evictLocked() {
	for {
		if s.maxEntries > 0 && len(s.entries) > s.maxEntries {
			s.deleteLocked(s.order[0])
			continue
		}
		if s.maxBytes > 0 && s.bytes > s.maxBytes && len(s.order) > 0 {
			s.deleteLocked(s.order[0])
			continue
		}
		break
	}
}

func entryBytes(key string, entry models.CacheEntry) int64 {
	total := int64(len(key))
	for path, file := range entry {
		total += int64(len(path) + len(file.Data) + len(file.Encoding))
	}
	return total
}

type RecordSet struct {
	RFP    *atproto.StrongRef
	Bid    *atproto.StrongRef
	Accept *atproto.StrongRef
}

func RecordKey(ref atproto.StrongRef) string {
	return fmt.Sprintf("rec/%s/%s", ref.URI, ref.CID)
}

func ReadRecord(store *Store, ref atproto.StrongRef) (map[string]any, bool) {
	entry, ok := store.Get(RecordKey(ref))
	if !ok {
		return nil, false
	}
	file, ok := entry[RecordValueFile]
	if !ok {
		return nil, false
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(file.Data), &value); err != nil {
		return nil, false
	}
	return value, true
}

func ReadRecordCache(seed models.Cache, ref atproto.StrongRef) (map[string]any, bool) {
	if len(seed) == 0 || ref.URI == "" {
		return nil, false
	}
	entry, ok := seed[RecordKey(ref)]
	if !ok {
		return nil, false
	}
	file, ok := entry[RecordValueFile]
	if !ok {
		return nil, false
	}
	data := []byte(file.Data)
	if file.Encoding == models.CacheEncodingBase64 {
		decoded, err := base64.StdEncoding.DecodeString(file.Data)
		if err != nil {
			return nil, false
		}
		data = decoded
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, false
	}
	return value, true
}

func WriteRecord(store *Store, ref atproto.StrongRef, value map[string]any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	store.Set(RecordKey(ref), models.CacheEntry{
		RecordValueFile: {Data: string(data), Encoding: models.CacheEncodingText},
	})
}

func policyRecordPart(ref *atproto.StrongRef) string {
	if ref == nil || ref.URI == "" {
		return ""
	}
	return ref.URI + "/" + ref.CID
}

func PolicyKey(name string, records RecordSet, args map[string]any) string {
	suffix := ""
	if len(args) > 0 {
		if data, err := json.Marshal(args); err == nil {
			suffix = ":" + HashString(string(data))
		}
	}
	return fmt.Sprintf("policy/%s/%s|%s|%s%s",
		name,
		policyRecordPart(records.RFP),
		policyRecordPart(records.Bid),
		policyRecordPart(records.Accept),
		suffix,
	)
}

type PolicyMeta struct {
	CachedAt int64 `json:"cachedAt"`
	TTLMs    int64 `json:"ttlMs"`
}

func ReadVerdict(store *Store, key string) ([]byte, bool) {
	entry, ok := store.Get(key)
	if !ok {
		return nil, false
	}
	if meta, ok := entry[PolicyMetaFile]; ok {
		var value PolicyMeta
		if err := json.Unmarshal([]byte(meta.Data), &value); err == nil {
			if time.Now().UnixMilli()-value.CachedAt > value.TTLMs {
				store.Delete(key)
				return nil, false
			}
		}
	}
	file, ok := entry[PolicyResultFile]
	if !ok {
		return nil, false
	}
	return []byte(file.Data), true
}

func WriteVerdict(store *Store, key string, data []byte, ttl time.Duration) {
	ttl = opt.Positive(ttl, 30*time.Second)
	meta, err := json.Marshal(PolicyMeta{CachedAt: time.Now().UnixMilli(), TTLMs: ttl.Milliseconds()})
	if err != nil {
		return
	}
	store.Set(key, models.CacheEntry{
		PolicyResultFile: {Data: string(data), Encoding: models.CacheEncodingText},
		PolicyMetaFile:   {Data: string(meta), Encoding: models.CacheEncodingText},
	})
}

func HashString(value string) string {
	var hash uint32 = 5381
	for i := 0; i < len(value); i++ {
		hash = (hash << 5) + hash + uint32(value[i])
	}
	return encodeBase36(uint64(hash))
}

func encodeBase36(value uint64) string {
	if value == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buffer [16]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%36]
		value /= 36
	}
	return string(buffer[index:])
}

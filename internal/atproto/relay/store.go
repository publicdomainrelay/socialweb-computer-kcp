package relay

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

type HostState string

const (
	HostActive     HostState = "active"
	HostIdle       HostState = "idle"
	HostConnecting HostState = "connecting"
	HostTakedown   HostState = "takedown"
)

type HostInfo struct {
	Hostname    string    `json:"hostname"`
	DID         string    `json:"did"`
	State       HostState `json:"state"`
	LastSeen    int64     `json:"lastSeen"`
	Cursor      *int64    `json:"cursor"`
	ConnectedAt *int64    `json:"connectedAt"`
	RetryCount  int       `json:"retryCount"`
}

type AccountInfo struct {
	DID          string  `json:"did"`
	HostHostname string  `json:"hostHostname"`
	Rev          *string `json:"rev"`
	Seq          *int64  `json:"seq"`
}

type Store interface {
	ListHosts() []HostInfo
	GetHost(hostname string) (HostInfo, bool)
	PutHost(info HostInfo)
	ListAccounts() []AccountInfo
	GetAccount(did string) (AccountInfo, bool)
	PutAccount(info AccountInfo)
	AddCollection(collection, did string)
	ListCollection(collection, cursor string, limit int) ([]string, string)
}

type MemoryStore struct {
	mu          sync.RWMutex
	hosts       map[string]HostInfo
	accounts    map[string]AccountInfo
	collections map[string]map[string]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		hosts:       map[string]HostInfo{},
		accounts:    map[string]AccountInfo{},
		collections: map[string]map[string]struct{}{},
	}
}

func (s *MemoryStore) ListHosts() []HostInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]HostInfo, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}

func (s *MemoryStore) GetHost(hostname string) (HostInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.hosts[hostname]
	return h, ok
}

func (s *MemoryStore) PutHost(info HostInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[info.Hostname] = info
}

func (s *MemoryStore) ListAccounts() []AccountInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AccountInfo, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DID < out[j].DID })
	return out
}

func (s *MemoryStore) GetAccount(did string) (AccountInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[did]
	return a, ok
}

func (s *MemoryStore) PutAccount(info AccountInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[info.DID] = info
}

func (s *MemoryStore) AddCollection(collection, did string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.collections[collection]
	if !ok {
		set = map[string]struct{}{}
		s.collections[collection] = set
	}
	set[did] = struct{}{}
}

func (s *MemoryStore) ListCollection(collection, cursor string, limit int) ([]string, string) {
	s.mu.RLock()
	all := make([]string, 0, len(s.collections[collection]))
	for did := range s.collections[collection] {
		all = append(all, did)
	}
	s.mu.RUnlock()
	sort.Strings(all)
	start := 0
	if cursor != "" {
		if last, ok := decodeCollectionCursor(cursor); ok {
			start = sort.SearchStrings(all, last)
			for start < len(all) && all[start] <= last {
				start++
			}
		}
	}
	limit = opt.Positive(limit, 50)
	end := min(start+limit, len(all))
	page := append([]string{}, all[start:end]...)
	next := ""
	if end < len(all) && len(page) > 0 {
		next = encodeCollectionCursor(collection, page[len(page)-1])
	}
	return page, next
}

func encodeCollectionCursor(collection, did string) string {
	b, _ := json.Marshal([]string{"collection", collection, did})
	return base64.StdEncoding.EncodeToString(b)
}

func decodeCollectionCursor(cursor string) (string, bool) {
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return "", false
	}
	var key []string
	if err := json.Unmarshal(b, &key); err != nil || len(key) != 3 {
		return "", false
	}
	return key[2], true
}

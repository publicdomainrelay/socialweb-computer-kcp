package component

import (
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
)

type Key = contract.Key

type Entry struct {
	Key        Key
	UID        types.UID
	Generation int64
	Endpoints  []Endpoint
	Service    Service
}

type Registry struct {
	mu      sync.RWMutex
	entries map[Key]Entry
	uids    map[types.UID]Key

	subMu sync.Mutex
	subs  map[uint64]func(Key)
	next  uint64
}

func (r *Registry) Subscribe(fn func(Key)) (cancel func()) {
	if r == nil || fn == nil {
		return func() {}
	}
	r.subMu.Lock()
	if r.subs == nil {
		r.subs = map[uint64]func(Key){}
	}
	r.next++
	id := r.next
	r.subs[id] = fn
	r.subMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.subMu.Lock()
			delete(r.subs, id)
			r.subMu.Unlock()
		})
	}
}

func (r *Registry) notify(keys ...Key) {
	if len(keys) == 0 {
		return
	}
	r.subMu.Lock()
	fns := make([]func(Key), 0, len(r.subs))
	for _, fn := range r.subs {
		fns = append(fns, fn)
	}
	r.subMu.Unlock()
	for _, key := range keys {
		for _, fn := range fns {
			fn(key)
		}
	}
}

func NewRegistry() *Registry {
	return &Registry{
		entries: map[Key]Entry{},
		uids:    map[types.UID]Key{},
	}
}

func (r *Registry) Put(entry Entry) {
	if r == nil {
		return
	}
	r.notify(r.put(entry)...)
}

func (r *Registry) put(entry Entry) []Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := []Key{entry.Key}
	if r.entries == nil {
		r.entries = map[Key]Entry{}
	}
	if r.uids == nil {
		r.uids = map[types.UID]Key{}
	}
	if old, ok := r.entries[entry.Key]; ok && old.UID != entry.UID {
		if key, ok := r.uids[old.UID]; ok && key == entry.Key {
			delete(r.uids, old.UID)
		}
	}
	if entry.UID != "" {
		if oldKey, ok := r.uids[entry.UID]; ok && oldKey != entry.Key {
			delete(r.entries, oldKey)
			changed = append(changed, oldKey)
		}
		r.uids[entry.UID] = entry.Key
	}
	entry.Endpoints = cloneEndpoints(entry.Endpoints)
	r.entries[entry.Key] = entry
	return changed
}

func (r *Registry) Get(key Key) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[key]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(entry), true
}

func (r *Registry) Lookup(workspace, kind, name string) (Entry, bool) {
	return r.Get(Key{Workspace: workspace, Kind: kind, Name: name})
}

func (r *Registry) Service(workspace, kind, name string) (Service, bool) {
	entry, ok := r.Lookup(workspace, kind, name)
	if !ok || entry.Service == nil {
		return nil, false
	}
	return entry.Service, true
}

func (r *Registry) Endpoints(workspace, kind, name string) []Endpoint {
	entry, ok := r.Lookup(workspace, kind, name)
	if !ok {
		return nil
	}
	return entry.Endpoints
}

func (r *Registry) Endpoint(workspace, kind, name, endpoint string) (Endpoint, bool) {
	for _, e := range r.Endpoints(workspace, kind, name) {
		if e.Name == endpoint {
			return e, true
		}
	}
	return Endpoint{}, false
}

func (r *Registry) URL(workspace, kind, name, endpoint string) (string, bool) {
	e, ok := r.Endpoint(workspace, kind, name, endpoint)
	if !ok || e.URL == "" {
		return "", false
	}
	return e.URL, true
}

func (r *Registry) Has(workspace, kind, name string) bool {
	if r == nil {
		return false
	}
	workspace = strings.TrimSpace(workspace)
	kind = strings.TrimSpace(kind)
	name = strings.TrimSpace(name)
	if workspace == "" && kind == "" && name == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key := range r.entries {
		if workspace != "" && key.Workspace != workspace {
			continue
		}
		if kind != "" && !strings.EqualFold(key.Kind, kind) {
			continue
		}
		if name != "" && !strings.EqualFold(key.Name, name) {
			continue
		}
		return true
	}
	return false
}

func (r *Registry) List(workspace string) []Entry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]Entry, 0, len(r.entries))
	for _, entry := range r.entries {
		if workspace != "" && entry.Key.Workspace != workspace {
			continue
		}
		out = append(out, cloneEntry(entry))
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Workspace != out[j].Key.Workspace {
			return out[i].Key.Workspace < out[j].Key.Workspace
		}
		if out[i].Key.Kind != out[j].Key.Kind {
			return out[i].Key.Kind < out[j].Key.Kind
		}
		return out[i].Key.Name < out[j].Key.Name
	})
	return out
}

func (r *Registry) Unregister(key Key, uid types.UID) bool {
	if r == nil {
		return false
	}
	if !r.unregister(key, uid) {
		return false
	}
	r.notify(key)
	return true
}

func (r *Registry) unregister(key Key, uid types.UID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok {
		return false
	}
	if uid != "" && entry.UID != uid {
		return false
	}
	delete(r.entries, key)
	if entry.UID != "" {
		if mapped, ok := r.uids[entry.UID]; ok && mapped == key {
			delete(r.uids, entry.UID)
		}
	}
	return true
}

func (r *Registry) DeleteUID(uid types.UID) bool {
	if r == nil {
		return false
	}
	key, ok := r.deleteUID(uid)
	if ok {
		r.notify(key)
	}
	return ok
}

func (r *Registry) deleteUID(uid types.UID) (Key, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.uids[uid]
	if !ok {
		return Key{}, false
	}
	entry, ok := r.entries[key]
	if ok && entry.UID == uid {
		delete(r.entries, key)
	}
	delete(r.uids, uid)
	return key, ok
}

func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

func cloneEntry(entry Entry) Entry {
	entry.Endpoints = cloneEndpoints(entry.Endpoints)
	return entry
}

func cloneEndpoints(endpoints []Endpoint) []Endpoint {
	if endpoints == nil {
		return nil
	}
	return append([]Endpoint(nil), endpoints...)
}

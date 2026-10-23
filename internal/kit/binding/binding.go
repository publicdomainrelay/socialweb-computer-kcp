package binding

import (
	"maps"
	"slices"
	"strings"
	"sync"
)

type Target struct {
	Cluster string

	Workspace string

	Name string
}

type Table[B any] struct {
	mu sync.RWMutex

	items map[string]B

	users map[string]map[string]Target
}

func New[B any]() *Table[B] {
	return &Table[B]{items: map[string]B{}, users: map[string]map[string]Target{}}
}

func Key(workspace string, parts ...string) string {
	out := strings.TrimSpace(workspace)
	for _, part := range parts {
		out += "/" + strings.ToLower(strings.TrimSpace(part))
	}
	return out
}

func (t *Table[B]) Set(target Target, value *B, refs ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := Key(target.Workspace, target.Name)
	if value == nil {
		delete(t.items, k)
	} else {
		t.items[k] = *value
	}
	for ref, set := range t.users {
		if !slices.Contains(refs, ref) {
			delete(set, k)
			if len(set) == 0 {
				delete(t.users, ref)
			}
		}
	}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		set := t.users[ref]
		if set == nil {
			set = map[string]Target{}
			t.users[ref] = set
		}
		set[k] = target
	}
}

func (t *Table[B]) Get(workspace, name string) (B, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	value, ok := t.items[Key(workspace, name)]
	return value, ok
}

func (t *Table[B]) Users(ref string) []Target {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return slices.Collect(maps.Values(t.users[ref]))
}

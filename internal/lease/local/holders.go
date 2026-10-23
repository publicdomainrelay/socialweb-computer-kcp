package local

import (
	"sort"
	"strings"
	"sync"
)

type Target struct {
	Cluster string

	Name string
}

type Placement struct {
	Cluster string

	Workspace string

	Key string
}

type engagement struct {
	cluster string

	clusters map[string]bool
}

type Holders struct {
	mu sync.Mutex

	engaged map[string]*engagement

	keys map[Target][]string

	holders map[string]map[Target]bool
}

func NewHolders() *Holders {
	return &Holders{engaged: map[string]*engagement{}, keys: map[Target][]string{}, holders: map[string]map[Target]bool{}}
}

func (h *Holders) Engage(workspace, cluster, name string) {
	workspace, cluster, name = strings.TrimSpace(workspace), strings.TrimSpace(cluster), strings.TrimSpace(name)
	if workspace == "" || cluster == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.engaged[workspace]
	if e == nil || e.cluster != cluster {
		e = &engagement{cluster: cluster, clusters: map[string]bool{}}
		h.engaged[workspace] = e
	}
	if name != "" {
		e.clusters[name] = true
	}
}

func (h *Holders) Hold(t Target, keys ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(t)
	kept := unique(keys)
	if len(kept) == 0 {
		return
	}
	h.keys[t] = kept
	for _, k := range kept {
		if h.holders[k] == nil {
			h.holders[k] = map[Target]bool{}
		}
		h.holders[k][t] = true
	}
}

func (h *Holders) Drop(t Target, extra ...string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := unique(append(h.dropLocked(t), extra...))
	free := keys[:0]
	for _, k := range keys {
		if len(h.holders[k]) == 0 {
			free = append(free, k)
		}
	}
	return free
}

func (h *Holders) Held(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holders[key]) > 0
}

func (h *Holders) Pending(workspace string, keys []string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pendingLocked(workspace, keys)
}

func (h *Holders) Placements(keys []string) []Placement {
	h.mu.Lock()
	defer h.mu.Unlock()
	workspaces := make([]string, 0, len(h.engaged))
	for ws := range h.engaged {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)
	var out []Placement
	for _, ws := range workspaces {
		for _, k := range h.pendingLocked(ws, keys) {
			out = append(out, Placement{Cluster: h.engaged[ws].cluster, Workspace: ws, Key: k})
		}
	}
	return out
}

func (h *Holders) pendingLocked(workspace string, keys []string) []string {
	e := h.engaged[strings.TrimSpace(workspace)]
	if e == nil {
		return nil
	}
	var out []string
	for _, k := range unique(keys) {
		home, ok := ParseKey(k)
		if !ok || !home.In(workspace, e.clusters) || len(h.holders[k]) > 0 {
			continue
		}
		out = append(out, k)
	}
	return out
}

func (h *Holders) dropLocked(t Target) []string {
	keys := h.keys[t]
	delete(h.keys, t)
	for _, k := range keys {
		delete(h.holders[k], t)
		if len(h.holders[k]) == 0 {
			delete(h.holders, k)
		}
	}
	return keys
}

func unique(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

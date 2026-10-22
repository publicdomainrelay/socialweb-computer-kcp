package registry

import (
	"sort"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
)

type Registry struct {
	mu        sync.RWMutex
	executors map[string]executor.Executor
}

func New(executors ...executor.Executor) *Registry {
	registry := &Registry{executors: make(map[string]executor.Executor)}
	for _, item := range executors {
		registry.Register(item)
	}
	return registry
}

func (r *Registry) Register(item executor.Executor) {
	if item == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executors[item.Kind()] = item
}

func (r *Registry) Get(kind string) (executor.Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.executors[kind]
	return item, ok
}

func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.executors))
	for kind := range r.executors {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func GhaLiteOnly(ghaLite executor.Executor) *Registry {
	registry := New(ghaLite)
	registry.Register(executor.NewDeny(
		executor.PolicyTypescriptNSID,
		"typescript policy records are disabled; the policy engine is gha-lite only",
	))
	registry.Register(executor.NewDeny(
		executor.PolicyDenoWorkerNSID,
		"denoWorker policy records are disabled; the policy engine is gha-lite only",
	))
	return registry
}

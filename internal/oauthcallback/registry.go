package oauthcallback

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

type Handler func(ctx context.Context, w http.ResponseWriter, r *http.Request) error

type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

func (r *Registry) Register(name string, handler Handler) {
	name = strings.TrimSpace(name)
	if r == nil || name == "" || handler == nil {
		return
	}
	r.mu.Lock()
	r.handlers[name] = handler
	r.mu.Unlock()
}

func (r *Registry) Lookup(name string) (Handler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	handler, ok := r.handlers[strings.TrimSpace(name)]
	r.mu.RUnlock()
	return handler, ok
}

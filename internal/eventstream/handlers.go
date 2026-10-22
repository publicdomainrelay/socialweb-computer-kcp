package eventstream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

type Event struct {
	Source string
	Seq    int64
	Repo   syntax.DID
	Commit string
	Rev    string
	Time   time.Time
	Ops    []pdsclient.RepoOp
}

type Handler interface {
	HandleEvent(ctx context.Context, event Event) error
}

type HandlerFunc func(ctx context.Context, event Event) error

func (f HandlerFunc) HandleEvent(ctx context.Context, event Event) error {
	if f == nil {
		return errors.New("eventstream: nil handler")
	}
	return f(ctx, event)
}

type HandlerRegistry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{handlers: map[string]Handler{}}
}

func (r *HandlerRegistry) Register(name string, handler Handler) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("eventstream: a handler needs a name")
	}
	if handler == nil {
		return errors.New("eventstream: a handler is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[string]Handler{}
	}
	if _, ok := r.handlers[name]; ok {
		return errors.New("eventstream: the handler " + name + " is registered twice")
	}
	r.handlers[name] = handler
	return nil
}

func (r *HandlerRegistry) Lookup(name string) (Handler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[strings.TrimSpace(name)]
	return handler, ok
}

func (r *HandlerRegistry) Remove(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name = strings.TrimSpace(name)
	if _, ok := r.handlers[name]; !ok {
		return false
	}
	delete(r.handlers, name)
	return true
}

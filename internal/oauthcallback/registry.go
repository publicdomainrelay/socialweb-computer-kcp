package oauthcallback

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

type Handler func(ctx context.Context, w http.ResponseWriter, r *http.Request) error

var (
	mu       sync.RWMutex
	handlers = map[string]Handler{}
)

func Register(name string, handler Handler) {
	name = strings.TrimSpace(name)
	if name == "" || handler == nil {
		return
	}
	mu.Lock()
	handlers[name] = handler
	mu.Unlock()
}

func Lookup(name string) (Handler, bool) {
	mu.RLock()
	handler, ok := handlers[strings.TrimSpace(name)]
	mu.RUnlock()
	return handler, ok
}

var (
	cliMu     sync.RWMutex
	cliPublic string
)

func SetCLIPublicKey(multibase string) {
	cliMu.Lock()
	cliPublic = strings.TrimSpace(multibase)
	cliMu.Unlock()
}

func CLIPublicKey() string {
	cliMu.RLock()
	defer cliMu.RUnlock()
	return cliPublic
}

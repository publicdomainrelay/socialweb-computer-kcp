package exposurelocal

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type Registry struct {
	mu sync.RWMutex

	routes map[string]route
}

type route struct {
	host string

	path string

	handler http.Handler
}

func NewRegistry() *Registry { return &Registry{routes: map[string]route{}} }

func (r *Registry) Register(owner, host, path string, handler http.Handler) error {
	if r == nil {
		return errors.New("exposurelocal: the handler registry is nil")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return errors.New("exposurelocal: a route needs an owner")
	}
	if handler == nil {
		return errors.New("exposurelocal: a route needs a handler")
	}
	host = normalizeHost(host)
	if host == "" {
		return errors.New("exposurelocal: a route needs a host")
	}
	r.mu.Lock()
	if r.routes == nil {
		r.routes = map[string]route{}
	}
	r.routes[owner] = route{host: host, path: normalizePath(path), handler: handler}
	r.mu.Unlock()
	return nil
}

func (r *Registry) Remove(owner string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.routes, owner)
	r.mu.Unlock()
}

func (r *Registry) Lookup(rawHost, rawPath string) (http.Handler, bool) {
	if r == nil {
		return nil, false
	}
	host := normalizeHost(rawHost)
	if host == "" {
		return nil, false
	}
	target := normalizePath(rawPath)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		best    http.Handler
		bestLen = -1
		bestOwn string
	)
	for owner, rt := range r.routes {
		if rt.host != host || !pathMatches(rt.path, target) {
			continue
		}
		if len(rt.path) > bestLen || (len(rt.path) == bestLen && owner < bestOwn) {
			best, bestLen, bestOwn = rt.handler, len(rt.path), owner
		}
	}
	return best, best != nil
}

func (r *Registry) HandlerFor(rawHost, rawPath string) (http.Handler, bool) {
	return r.Lookup(rawHost, rawPath)
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	handler, ok := r.Lookup(req.Host, req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}
	handler.ServeHTTP(w, req)
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" || port == "80" || port == "443" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func pathMatches(prefix, target string) bool {
	if prefix == "/" {
		return true
	}
	return target == prefix || strings.HasPrefix(target, prefix+"/")
}

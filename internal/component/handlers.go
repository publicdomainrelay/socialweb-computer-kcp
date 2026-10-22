package component

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

const DefaultHandlerName = "http"

var (
	ErrNoHandler       = errors.New("component: no HTTP handler is registered")
	ErrNoStream        = errors.New("component: no stream handler is registered")
	ErrHandlersStopped = errors.New("component: the handler registry is stopped")
)

type StreamHandler func(ctx context.Context, conn net.Conn) error

type HandlerRegistry struct {
	mu sync.RWMutex

	handlers  map[Key]map[string]http.Handler
	revisions map[Key]map[string]uint64
	aliases   map[string]handlerAlias
	streams   map[streamKey]StreamHandler
	conns     map[*trackedConn]Key

	root   context.Context
	cancel context.CancelFunc
	closed bool

	transport *http.Transport
}

type Handlers = HandlerRegistry

type handlerAlias struct {
	key       Key
	name      string
	ambiguous bool
}

type streamKey struct {
	Key
	name string
}

func NewHandlerRegistry() *HandlerRegistry {
	root, cancel := context.WithCancel(context.Background())
	r := &HandlerRegistry{
		handlers:  map[Key]map[string]http.Handler{},
		revisions: map[Key]map[string]uint64{},
		aliases:   map[string]handlerAlias{},
		streams:   map[streamKey]StreamHandler{},
		conns:     map[*trackedConn]Key{},
		root:      root,
		cancel:    cancel,
	}
	r.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return r.dialHTTP(network, address)
		},
	}
	return r
}

func NewHandlers() *HandlerRegistry {
	return NewHandlerRegistry()
}

func (r *HandlerRegistry) Client() *http.Client {
	if r == nil {
		return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, ErrHandlersStopped
		})}
	}
	return &http.Client{Transport: r.RoundTripper()}
}

func (r *HandlerRegistry) HTTPClient() *http.Client {
	return r.Client()
}

func (r *HandlerRegistry) RoundTripper() http.RoundTripper {
	return &handlerRoundTripper{registry: r}
}

func (r *HandlerRegistry) RegisterHTTP(key Key, handler http.Handler, name ...string) error {
	return r.RegisterHTTPNamed(key, optionalName(name), handler)
}

func (r *HandlerRegistry) RegisterHTTPNamed(key Key, name string, handler http.Handler) error {
	if r == nil {
		return errors.New("component: the handler registry is nil")
	}
	if handler == nil {
		return fmt.Errorf("component: the HTTP handler for %s/%s is nil", key.Kind, key.Name)
	}
	if strings.TrimSpace(key.Kind) == "" || strings.TrimSpace(key.Name) == "" {
		return errors.New("component: an HTTP handler key needs a kind and a name")
	}
	name = normalizeHandlerName(name)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrHandlersStopped
	}
	if r.handlers == nil {
		r.handlers = map[Key]map[string]http.Handler{}
	}
	names := r.handlers[key]
	if names == nil {
		names = map[string]http.Handler{}
		r.handlers[key] = names
	}
	names[name] = handler
	revs := r.revisions[key]
	if revs == nil {
		revs = map[string]uint64{}
		r.revisions[key] = revs
	}
	revs[name]++
	r.rebuildAliasesLocked()
	stale := r.takeConnsLocked(key)
	r.mu.Unlock()
	closeTracked(stale)
	return nil
}

func (r *HandlerRegistry) Handler(key Key, name ...string) (http.Handler, bool) {
	return r.HandlerNamed(key, optionalName(name))
}

func (r *HandlerRegistry) HandlerRevision(key Key, name ...string) uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revisions[key][normalizeHandlerName(optionalName(name))]
}

func (r *HandlerRegistry) HandlerNamed(key Key, name string) (http.Handler, bool) {
	if r == nil {
		return nil, false
	}
	name = normalizeHandlerName(name)
	r.mu.RLock()
	handler := r.handlers[key][name]
	r.mu.RUnlock()
	return handler, handler != nil
}

func (r *HandlerRegistry) HandlerForHost(host string) (http.Handler, bool) {
	handler, _, ok := r.handlerForHost(host)
	return handler, ok
}

func (r *HandlerRegistry) DialHTTP(ctx context.Context, address string) (net.Conn, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	return r.dialHTTP("tcp", address)
}

func (r *HandlerRegistry) Remove(key Key) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	removed := false
	if _, ok := r.handlers[key]; ok {
		delete(r.handlers, key)
		delete(r.revisions, key)
		removed = true
	}
	for streamKey := range r.streams {
		if streamKey.Key == key {
			delete(r.streams, streamKey)
			removed = true
		}
	}
	if removed {
		r.rebuildAliasesLocked()
	}
	stale := r.takeConnsLocked(key)
	r.mu.Unlock()
	closeTracked(stale)
	return removed
}

func (r *HandlerRegistry) RegisterStream(key Key, handler StreamHandler, name ...string) error {
	if r == nil {
		return errors.New("component: the handler registry is nil")
	}
	if handler == nil {
		return fmt.Errorf("component: the stream handler for %s/%s is nil", key.Kind, key.Name)
	}
	if strings.TrimSpace(key.Kind) == "" || strings.TrimSpace(key.Name) == "" {
		return errors.New("component: a stream handler key needs a kind and a name")
	}
	sk := streamKey{Key: key, name: normalizeHandlerName(optionalName(name))}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrHandlersStopped
	}
	if r.streams == nil {
		r.streams = map[streamKey]StreamHandler{}
	}
	r.streams[sk] = handler
	stale := r.takeConnsLocked(key)
	r.mu.Unlock()
	closeTracked(stale)
	return nil
}

func (r *HandlerRegistry) DialStream(ctx context.Context, key Key, name ...string) (net.Conn, error) {
	if r == nil {
		return nil, ErrHandlersStopped
	}
	sk := streamKey{Key: key, name: normalizeHandlerName(optionalName(name))}
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, ErrHandlersStopped
	}
	handler := r.streams[sk]
	root := r.root
	r.mu.RUnlock()
	if handler == nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrNoStream, key.Kind, key.Name)
	}
	client, server := net.Pipe()
	clientConn, err := r.trackConn(client, key)
	if err != nil {
		server.Close()
		return nil, err
	}
	serverConn, err := r.trackConn(server, key)
	if err != nil {
		clientConn.Close()
		server.Close()
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stopRoot := context.AfterFunc(root, cancel)
	clientConn.addHook(func() {
		stopRoot()
	})
	stopClient := context.AfterFunc(ctx, func() {
		clientConn.Close()
	})
	clientConn.addHook(func() {
		stopClient()
	})
	go func() {
		defer cancel()
		defer serverConn.Close()
		_ = handler(streamCtx, serverConn)
	}()
	return clientConn, nil
}

func (r *HandlerRegistry) RemoveStream(key Key, name ...string) bool {
	if r == nil {
		return false
	}
	sk := streamKey{Key: key, name: normalizeHandlerName(optionalName(name))}
	r.mu.Lock()
	_, ok := r.streams[sk]
	delete(r.streams, sk)
	stale := r.takeConnsLocked(key)
	r.mu.Unlock()
	closeTracked(stale)
	return ok
}

func (r *HandlerRegistry) Ready(ctx ...context.Context) error {
	if r == nil {
		return ErrHandlersStopped
	}
	if len(ctx) > 0 && ctx[0] != nil {
		if err := ctx[0].Err(); err != nil {
			return err
		}
	}
	r.mu.RLock()
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return ErrHandlersStopped
	}
	return nil
}

func (r *HandlerRegistry) Stop(ctx ...context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	stale := make([]*trackedConn, 0, len(r.conns))
	for conn := range r.conns {
		stale = append(stale, conn)
	}
	r.conns = map[*trackedConn]Key{}
	r.handlers = map[Key]map[string]http.Handler{}
	r.streams = map[streamKey]StreamHandler{}
	r.aliases = map[string]handlerAlias{}
	r.mu.Unlock()
	closeTracked(stale)
	return nil
}

func (r *HandlerRegistry) Close() error {
	return r.Stop()
}

func (r *HandlerRegistry) Closed() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closed
}

func (r *HandlerRegistry) dialHTTP(_ string, address string) (net.Conn, error) {
	if err := r.Ready(); err != nil {
		return nil, err
	}
	host := normalizeHost(address)
	handler, key, ok := r.handlerForHost(host)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoHandler, host)
	}
	client, server := net.Pipe()
	clientConn, err := r.trackConn(client, key)
	if err != nil {
		server.Close()
		return nil, err
	}
	serverConn, err := r.trackConn(server, key)
	if err != nil {
		clientConn.Close()
		server.Close()
		return nil, err
	}
	serveCtx, cancel := context.WithCancel(r.rootContext())
	go func() {
		defer cancel()
		r.serveHTTP(serveCtx, serverConn, handler)
	}()
	return clientConn, nil
}

func (r *HandlerRegistry) serveHTTP(ctx context.Context, conn *trackedConn, handler http.Handler) {
	defer conn.Close()
	server := &http.Server{
		Handler: handler,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	_ = server.Serve(newSingleConnListener(conn))
}

func (r *HandlerRegistry) handlerForHost(host string) (http.Handler, Key, bool) {
	if r == nil {
		return nil, Key{}, false
	}
	host = normalizeHost(host)
	r.mu.RLock()
	alias, ok := r.aliases[host]
	if ok && !alias.ambiguous {
		handler := r.handlers[alias.key][alias.name]
		r.mu.RUnlock()
		return handler, alias.key, handler != nil
	}
	r.mu.RUnlock()
	return nil, Key{}, false
}

func (r *HandlerRegistry) rootContext() context.Context {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.root == nil {
		return context.Background()
	}
	return r.root
}

func (r *HandlerRegistry) rebuildAliasesLocked() {
	aliases := make(map[string]handlerAlias, len(r.aliases))
	for key, names := range r.handlers {
		for name := range names {
			for _, host := range handlerAliases(key, name) {
				current, ok := aliases[host]
				if !ok {
					aliases[host] = handlerAlias{key: key, name: name}
					continue
				}
				if current.key != key || current.name != name {
					current.ambiguous = true
					aliases[host] = current
				}
			}
		}
	}
	r.aliases = aliases
}

func (r *HandlerRegistry) trackConn(conn net.Conn, key Key) (*trackedConn, error) {
	tracked := &trackedConn{Conn: conn, done: make(chan struct{})}
	tracked.onClose = func() {
		r.mu.Lock()
		delete(r.conns, tracked)
		r.mu.Unlock()
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		conn.Close()
		return nil, ErrHandlersStopped
	}
	if r.conns == nil {
		r.conns = map[*trackedConn]Key{}
	}
	r.conns[tracked] = key
	r.mu.Unlock()
	return tracked, nil
}

func (r *HandlerRegistry) takeConnsLocked(key Key) []*trackedConn {
	out := make([]*trackedConn, 0, len(r.conns))
	for conn, connKey := range r.conns {
		if connKey != key {
			continue
		}
		delete(r.conns, conn)
		out = append(out, conn)
	}
	return out
}

type trackedConn struct {
	net.Conn

	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	hooks  []func()
	closed bool

	onClose func()
}

func (c *trackedConn) addHook(hook func()) {
	if hook == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		hook()
		return
	}
	c.hooks = append(c.hooks, hook)
	c.mu.Unlock()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
		c.mu.Lock()
		c.closed = true
		hooks := c.hooks
		c.hooks = nil
		c.mu.Unlock()
		for _, hook := range hooks {
			hook()
		}
		close(c.done)
	})
	return err
}

type singleConnListener struct {
	conn   net.Conn
	done   <-chan struct{}
	mu     sync.Mutex
	served bool
}

func newSingleConnListener(conn *trackedConn) *singleConnListener {
	return &singleConnListener{conn: conn, done: conn.done}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.served {
		l.served = true
		conn := l.conn
		l.mu.Unlock()
		return conn, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

type handlerRoundTripper struct {
	registry *HandlerRegistry
}

func (t *handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.registry == nil {
		return nil, ErrHandlersStopped
	}
	if req == nil || req.URL == nil {
		return nil, errors.New("component: the HTTP request has no URL")
	}
	if req.URL.Scheme != "http" {
		return nil, fmt.Errorf("component: the in-process transport does not serve the scheme %q", req.URL.Scheme)
	}
	if err := t.registry.Ready(); err != nil {
		return nil, err
	}
	host := normalizeHost(requestHost(req))
	if _, _, ok := t.registry.handlerForHost(host); !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoHandler, host)
	}
	return t.registry.transport.RoundTrip(req)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func requestHost(req *http.Request) string {
	if req.URL != nil && req.URL.Host != "" {
		return req.URL.Host
	}
	return req.Host
}

func handlerAliases(key Key, name string) []string {
	kind := normalizeHost(key.Kind)
	component := normalizeHost(key.Name)
	if kind == "" || component == "" {
		return nil
	}
	bases := []string{kind, kind + "." + component}
	if workspace := strings.TrimSpace(key.Workspace); workspace != "" {
		bases = append(bases, kcp.ServiceFQDN(kind, "default", workspace, kcp.DefaultServiceDomain))
	}
	out := make([]string, 0, len(bases))
	for _, base := range bases {
		if name != DefaultHandlerName {
			base = "_" + name + "." + base
		}
		out = append(out, normalizeHost(base))
	}
	return out
}

func normalizeHost(value string) string {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")
	value = strings.TrimSuffix(value, ".")
	return strings.ToLower(value)
}

func normalizeHandlerName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return DefaultHandlerName
	}
	return name
}

func optionalName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func closeTracked(conns []*trackedConn) {
	for _, conn := range conns {
		conn.Close()
	}
}

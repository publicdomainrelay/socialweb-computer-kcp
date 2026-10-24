package bus

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

const DefaultHandlerName = "http"

var (
	ErrNoHandler       = errors.New("bus: no HTTP handler is registered")
	ErrNoStream        = errors.New("bus: no stream handler is registered")
	ErrHandlersStopped = errors.New("bus: the handler registry is stopped")
)

type StreamHandler func(ctx context.Context, conn net.Conn) error

type Bus struct {
	mu sync.RWMutex

	handlers  map[Key]map[string]http.Handler
	revisions map[Key]map[string]uint64
	aliases   map[string]handlerAlias
	streams   map[streamKey]StreamHandler
	conns     map[*trackedConn]Key
	links     []Link
	changed   chan struct{}
	providers map[providerKey]provider

	subMu   sync.Mutex
	subs    map[uint64]func(Key)
	nextSub uint64

	denials denials

	root   context.Context
	cancel context.CancelFunc
	closed bool
	served sync.WaitGroup

	transport *http.Transport
}

const LinkSettle = 5 * time.Second

type HandlerRegistry = Bus

type Handlers = Bus

type Key = contract.Key

type handlerAlias struct {
	key       Key
	name      string
	ambiguous bool
}

type streamKey struct {
	Key
	name string
}

func New(ctx context.Context) *Bus {
	root, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Bus{
		handlers:  map[Key]map[string]http.Handler{},
		revisions: map[Key]map[string]uint64{},
		aliases:   map[string]handlerAlias{},
		streams:   map[streamKey]StreamHandler{},
		conns:     map[*trackedConn]Key{},
		providers: map[providerKey]provider{},
		root:      root,
		cancel:    cancel,
	}
	r.denials.changed = make(chan struct{}, 1)
	r.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return r.dialHTTP(ctx, network, address)
		},
	}
	return r
}

func NewHandlerRegistry(ctx context.Context) *Bus {
	return New(ctx)
}

func (r *Bus) Subscribe(fn func(Key)) (cancel func()) {
	if r == nil || fn == nil {
		return func() {}
	}
	r.subMu.Lock()
	if r.subs == nil {
		r.subs = map[uint64]func(Key){}
	}
	r.nextSub++
	id := r.nextSub
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

func (r *Bus) notify(key Key) {
	r.subMu.Lock()
	fns := make([]func(Key), 0, len(r.subs))
	for _, fn := range r.subs {
		fns = append(fns, fn)
	}
	r.subMu.Unlock()
	for _, fn := range fns {
		fn(key)
	}
}

func NewHandlers(ctx context.Context) *Bus {
	return New(ctx)
}

func (r *Bus) SetLogger(log *slog.Logger) {
	if r == nil {
		return
	}
	r.denials.log.Store(log)
}

func (r *Bus) Denied() uint64 {
	if r == nil {
		return 0
	}
	return r.denials.count.Load()
}

func (r *Bus) DeniedByCapability() map[string]uint64 {
	out := map[string]uint64{}
	if r == nil {
		return out
	}
	r.denials.byCapability.Do(func(kv expvar.KeyValue) {
		if n, ok := kv.Value.(*expvar.Int); ok {
			out[kv.Key] = uint64(n.Value())
		}
	})
	return out
}

func (r *Bus) DeniedChanged() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.denials.changed
}

func (r *Bus) Client() *http.Client {
	if r == nil {
		return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, ErrHandlersStopped
		})}
	}
	return &http.Client{Transport: r.RoundTripper()}
}

func (r *Bus) HTTPClient() *http.Client {
	return r.Client()
}

func (r *Bus) RoundTripper() http.RoundTripper {
	return &handlerRoundTripper{registry: r}
}

func (r *Bus) SetLinks(links []Link) {
	if r == nil {
		return
	}
	next := cloneLinks(links)
	r.mu.Lock()
	if linksEqual(r.links, next) {
		r.mu.Unlock()
		return
	}
	r.links = next
	if r.changed != nil {
		close(r.changed)
		r.changed = nil
	}
	stale := make([]*trackedConn, 0, len(r.conns))
	for conn, key := range r.conns {
		if conn.granted && allowed(next, conn.caller, key.Ref(), conn.grant) {
			continue
		}
		delete(r.conns, conn)
		stale = append(stale, conn)
	}
	r.mu.Unlock()
	closeTracked(stale)
}

func (r *Bus) Links() []Link {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneLinks(r.links)
}

func (r *Bus) RegisterHTTP(key Key, handler http.Handler, name ...string) error {
	return r.RegisterHTTPNamed(key, optionalName(name), handler)
}

func (r *Bus) RegisterHTTPNamed(key Key, name string, handler http.Handler) error {
	if r == nil {
		return errors.New("bus: the handler registry is nil")
	}
	if handler == nil {
		return fmt.Errorf("bus: the HTTP handler for %s/%s is nil", key.Kind, key.Name)
	}
	if strings.TrimSpace(key.Kind) == "" || strings.TrimSpace(key.Name) == "" {
		return errors.New("bus: an HTTP handler key needs a kind and a name")
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
	r.notify(key)
	return nil
}

func (r *Bus) Handler(key Key, name ...string) (http.Handler, bool) {
	return r.HandlerNamed(key, optionalName(name))
}

func (r *Bus) HandlerRevision(key Key, name ...string) uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revisions[key][normalizeHandlerName(optionalName(name))]
}

func (r *Bus) HandlerNamed(key Key, name string) (http.Handler, bool) {
	if r == nil {
		return nil, false
	}
	name = normalizeHandlerName(name)
	r.mu.RLock()
	handler := r.handlers[key][name]
	r.mu.RUnlock()
	return handler, handler != nil
}

func (r *Bus) HandlerForHost(host string) (http.Handler, bool) {
	handler, _, _, ok := r.handlerForHost(host)
	return handler, ok
}

func (r *Bus) DialHTTP(ctx context.Context, address string) (net.Conn, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	return r.dialHTTP(ctx, "tcp", address)
}

func (r *Bus) Remove(key Key) bool {
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
	if removed {
		r.notify(key)
	}
	return removed
}

func (r *Bus) RegisterStream(key Key, handler StreamHandler, name ...string) error {
	if r == nil {
		return errors.New("bus: the handler registry is nil")
	}
	if handler == nil {
		return fmt.Errorf("bus: the stream handler for %s/%s is nil", key.Kind, key.Name)
	}
	if strings.TrimSpace(key.Kind) == "" || strings.TrimSpace(key.Name) == "" {
		return errors.New("bus: a stream handler key needs a kind and a name")
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
	r.notify(key)
	return nil
}

func (r *Bus) DialStream(ctx context.Context, key Key, name ...string) (net.Conn, error) {
	if r == nil {
		return nil, ErrHandlersStopped
	}
	handlerName := normalizeHandlerName(optionalName(name))
	grant := Capability{Name: CapabilityStream, Scope: []string{handlerName}}
	if err := r.authorize(ctx, key.Ref(), grant); err != nil {
		return nil, err
	}
	caller, hasCaller := CallerFrom(ctx)
	sk := streamKey{Key: key, name: handlerName}
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
	clientConn, err := r.trackGranted(client, key, caller, hasCaller, grant)
	if err != nil {
		server.Close()
		return nil, err
	}
	serverConn, err := r.trackGranted(server, key, caller, hasCaller, grant)
	if err != nil {
		clientConn.Close()
		server.Close()
		return nil, err
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
	if !r.goServe(func() {
		defer cancel()
		defer serverConn.Close()
		_ = handler(streamCtx, serverConn)
	}) {
		cancel()
		clientConn.Close()
		serverConn.Close()
		return nil, ErrHandlersStopped
	}
	return clientConn, nil
}

func (r *Bus) RemoveStream(key Key, name ...string) bool {
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
	if ok {
		r.notify(key)
	}
	return ok
}

func (r *Bus) Ready(ctx ...context.Context) error {
	if r == nil {
		return ErrHandlersStopped
	}
	if len(ctx) > 0 {
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

func (r *Bus) Stop(ctx ...context.Context) error {
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
	if len(ctx) > 0 {
		return supervise.Join(ctx[0], r.served.Wait)
	}
	r.served.Wait()
	return nil
}

func (r *Bus) goServe(fn func()) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.served.Go(fn)
	return true
}

func (r *Bus) Close() error {
	return r.Stop()
}

func (r *Bus) Closed() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closed
}

func (r *Bus) dialHTTP(ctx context.Context, _ string, address string) (net.Conn, error) {
	if err := r.Ready(); err != nil {
		return nil, err
	}
	host := hostport.Normalize(address)
	handler, key, _, ok := r.handlerForHost(host)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoHandler, host)
	}
	grant := Capability{Name: CapabilityXRPC}
	if authorized, ok := ctx.Value(authorizedDialKey{}).(authorizedDial); ok && authorized.key == key {
		grant = authorized.grant
	} else if err := r.authorize(ctx, key.Ref(), grant); err != nil {
		return nil, err
	}
	caller, hasCaller := CallerFrom(ctx)
	client, server := net.Pipe()
	clientConn, err := r.trackGranted(client, key, caller, hasCaller, grant)
	if err != nil {
		server.Close()
		return nil, err
	}
	serverConn, err := r.trackGranted(server, key, caller, hasCaller, grant)
	if err != nil {
		clientConn.Close()
		server.Close()
		return nil, err
	}
	serverConn.caller = caller
	serverConn.hasCaller = hasCaller
	serveCtx, cancel := context.WithCancel(r.rootContext())
	if !r.goServe(func() {
		defer cancel()
		r.serveHTTP(serveCtx, serverConn, handler)
	}) {
		cancel()
		clientConn.Close()
		serverConn.Close()
		return nil, ErrHandlersStopped
	}
	return clientConn, nil
}

func (r *Bus) serveHTTP(ctx context.Context, conn *trackedConn, handler http.Handler) {
	defer conn.Close()
	if conn.hasCaller {
		ctx = context.WithValue(ctx, callerKey{}, conn.caller)
	}
	server := &http.Server{
		Handler: handler,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	_ = server.Serve(newSingleConnListener(conn))
}

func (r *Bus) handlerForHost(host string) (http.Handler, Key, string, bool) {
	if r == nil {
		return nil, Key{}, "", false
	}
	host = hostport.Normalize(host)
	r.mu.RLock()
	alias, ok := r.aliases[host]
	if ok && !alias.ambiguous {
		handler := r.handlers[alias.key][alias.name]
		r.mu.RUnlock()
		return handler, alias.key, alias.name, handler != nil
	}
	r.mu.RUnlock()
	return nil, Key{}, "", false
}

func (r *Bus) rootContext() context.Context {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.root
}

func (r *Bus) rebuildAliasesLocked() {
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

func (r *Bus) trackConn(conn net.Conn, key Key) (*trackedConn, error) {
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

func (r *Bus) trackGranted(conn net.Conn, key Key, caller Caller, hasCaller bool, grant Capability) (*trackedConn, error) {
	tracked, err := r.trackConn(conn, key)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	tracked.caller, tracked.grant, tracked.granted = caller, grant, hasCaller
	r.mu.Unlock()
	return tracked, nil
}

func (r *Bus) takeConnsLocked(key Key) []*trackedConn {
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

	caller    Caller
	hasCaller bool

	grant   Capability
	granted bool

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
	registry *Bus
}

func (t *handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.registry == nil {
		return nil, ErrHandlersStopped
	}
	if req == nil || req.URL == nil {
		return nil, errors.New("bus: the HTTP request has no URL")
	}
	if req.URL.Scheme != "http" {
		return nil, fmt.Errorf("bus: the in-process transport does not serve the scheme %q", req.URL.Scheme)
	}
	if err := t.registry.Ready(); err != nil {
		return nil, err
	}
	host := hostport.Normalize(requestHost(req))
	_, key, _, ok := t.registry.handlerForHost(host)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoHandler, host)
	}
	grant := xrpcCapability(req)
	if err := t.registry.authorize(req.Context(), key.Ref(), grant); err != nil {
		return nil, err
	}
	next := req.Clone(context.WithValue(req.Context(), authorizedDialKey{}, authorizedDial{key: key, grant: grant}))
	next.Close = true
	return t.registry.transport.RoundTrip(next)
}

type authorizedDialKey struct{}

type authorizedDial struct {
	key Key

	grant Capability
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

func xrpcCapability(req *http.Request) Capability {
	if req != nil && req.URL != nil {
		if nsid, ok := strings.CutPrefix(req.URL.Path, "/xrpc/"); ok && nsid != "" && !strings.Contains(nsid, "/") {
			return Capability{Name: CapabilityXRPC, Scope: []string{nsid}}
		}
	}
	return Capability{Name: CapabilityXRPC}
}

func (r *Bus) authorize(ctx context.Context, to commonv1.ObjectRef, c Capability) error {
	if r == nil {
		return ErrHandlersStopped
	}
	from, ok := CallerFrom(ctx)
	if !ok {
		return r.denials.deny(Caller{}, to, c)
	}
	return r.authorizeCaller(ctx, from, to, c)
}

func (r *Bus) authorizeCaller(ctx context.Context, from Caller, to commonv1.ObjectRef, c Capability) error {
	wait := time.NewTimer(LinkSettle)
	defer wait.Stop()
	for {
		r.mu.Lock()
		links := r.links
		if r.changed == nil {
			r.changed = make(chan struct{})
		}
		changed := r.changed
		r.mu.Unlock()
		if allowed(links, from, to, c) {
			return nil
		}
		if from.System() || !from.Complete() {
			return r.denials.deny(from, to, c)
		}
		select {
		case <-changed:
		case <-wait.C:
			return r.denials.deny(from, to, c)
		case <-ctx.Done():
			return r.denials.deny(from, to, c)
		}
	}
}

func allowed(links []Link, from Caller, to commonv1.ObjectRef, c Capability) bool {
	if from.System() {
		return BootstrapCaller(from.Ref.Name)
	}
	if !from.Complete() {
		return false
	}
	if refMatches(from.Ref, to) {
		return true
	}
	for _, link := range links {
		if LinkAllows(link, from, to, c) {
			return true
		}
	}
	return false
}

func handlerAliases(key Key, name string) []string {
	kind := hostport.Normalize(key.Kind)
	component := hostport.Normalize(key.Name)
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
		out = append(out, hostport.Normalize(base))
	}
	return out
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

func (r *Bus) Transport(external http.RoundTripper) http.RoundTripper {
	return handlerTransport{bus: r, external: external}
}

type handlerTransport struct {
	bus *Bus

	external http.RoundTripper
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && req.URL.Scheme == "http" && t.bus != nil {
		if _, ok := t.bus.HandlerForHost(req.URL.Host); ok {
			return t.bus.RoundTripper().RoundTrip(req)
		}
	}
	external := t.external
	if external == nil {
		external = http.DefaultTransport
	}
	return external.RoundTrip(req)
}

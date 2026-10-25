package relay

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/gorilla/websocket"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

const SubscribePingInterval = 30 * time.Second

var (
	ErrInvalidHostname = errors.New("relay: hostname is required")
	ErrPDSNotFound     = errors.New("relay: could not resolve PDS identity")
	ErrHostNotAllowed  = errors.New("relay: host is outside the relay scope")
	ErrDIDNotAllowed   = errors.New("relay: DID is outside the relay scope")
	ErrClosed          = errors.New("relay: closed")
)

type Config struct {
	Hostname      string
	Version       string
	Logger        *slog.Logger
	Store         Store
	Directory     identity.Directory
	PLCURL        string
	InsecureHTTP  bool
	DisableVerify bool
	StrictVerify  bool
	MaxBacklog    int
	StartSeq      int64
	IdleAfter     time.Duration
	AllowedDIDs   []string
	AllowedHosts  []string
	HTTPClient    *http.Client
	DialContext   func(ctx context.Context, network, address string) (net.Conn, error)
	TLSConfig     *tls.Config
}

type EventSink interface {
	HandleEvent(ctx context.Context, ev Event) error
}

type Event struct {
	Seq    int64
	Frame  Frame
	Commit *comatproto.SyncSubscribeRepos_Commit
}

type Relay struct {
	cfg        Config
	store      Store
	seq        *Sequencer
	log        *slog.Logger
	dir        identity.Directory
	verify     *verifier
	httpClient *http.Client
	handler    http.Handler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	subs   map[string]*hostSub
	closed bool

	sinksMu sync.RWMutex
	sinks   []EventSink

	hostMu sync.Mutex
}

func New(ctx context.Context, cfg Config) *Relay {
	cfg.Hostname = cmp.Or(cfg.Hostname, "localhost")
	cfg.Version = cmp.Or(cfg.Version, "0.0.0")
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}
	if cfg.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = cfg.TLSConfig
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second, Transport: transport}
	}
	dir := cfg.Directory
	if dir == nil {
		if cfg.PLCURL == "" {
			dir = identity.DefaultDirectory()
		} else {
			base := &identity.BaseDirectory{
				PLCURL:                 strings.TrimSuffix(cfg.PLCURL, "/"),
				HTTPClient:             http.Client{Timeout: 10 * time.Second},
				PLCClient:              &http.Client{Timeout: 10 * time.Second},
				SkipHandleVerification: true,
			}
			dir = identity.NewCacheDirectory(base, 100_000, time.Hour, 2*time.Minute, 5*time.Minute)
		}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Relay{
		cfg:        cfg,
		store:      cfg.Store,
		seq:        NewSequencer(cfg.MaxBacklog),
		log:        cfg.Logger,
		dir:        dir,
		httpClient: cfg.HTTPClient,
		ctx:        ctx,
		cancel:     cancel,
		subs:       map[string]*hostSub{},
	}
	r.seq.Resume(cfg.StartSeq)
	r.verify = &verifier{dir: dir, log: r.log}
	r.handler = withCORS(http.HandlerFunc(r.serveHTTP))
	return r
}

func (r *Relay) Handler() http.Handler { return r.handler }

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *Relay) Store() Store { return r.store }

func (r *Relay) Sequencer() *Sequencer { return r.seq }

func (r *Relay) Directory() identity.Directory { return r.dir }

func (r *Relay) AddSink(s EventSink) {
	if s == nil {
		return
	}
	r.sinksMu.Lock()
	r.sinks = append(r.sinks, s)
	r.sinksMu.Unlock()
}

func (r *Relay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	subs := make([]*hostSub, 0, len(r.subs))
	for _, s := range r.subs {
		subs = append(subs, s)
	}
	r.subs = map[string]*hostSub{}
	r.mu.Unlock()
	r.cancel()
	for _, s := range subs {
		s.stop()
	}
	r.wg.Wait()
}

func (r *Relay) serveHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && path == "/xrpc/_health":
		httpjson.Write(w, http.StatusOK, map[string]string{"version": r.cfg.Version})
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.server.describeServer":
		httpjson.Write(w, http.StatusOK, map[string]any{
			"did":                  hostport.WebDID(r.cfg.Hostname),
			"version":              r.cfg.Version,
			"availableUserDomains": []string{},
			"inviteCodeRequired":   false,
		})

	case req.Method == http.MethodGet && path == "/.well-known/atproto-did":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(hostport.WebDID(r.cfg.Hostname)))
	case req.Method == http.MethodGet && path == "/.well-known/did.json":
		host := hostport.HostnameOnly(hostport.FromRequest(req, r.cfg.Hostname))
		httpjson.Write(w, http.StatusOK, map[string]any{
			"@context": []string{"https://www.w3.org/ns/did/v1"},
			"id":       hostport.WebDID(host),
			"service": []map[string]string{{
				"id":              "#atproto_relay",
				"type":            "AtprotoRelay",
				"serviceEndpoint": "https://" + host,
			}},
		})

	case req.Method == http.MethodPost && path == "/xrpc/com.atproto.sync.requestCrawl":
		r.handleRequestCrawl(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.subscribeRepos":
		r.handleSubscribeRepos(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.getRepo":
		r.handleGetRepo(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.getRepoStatus":
		r.handleGetRepoStatus(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.listRepos":
		r.handleListRepos(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.listReposByCollection":
		r.handleListReposByCollection(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.getLatestCommit":
		r.handleGetLatestCommit(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.listHosts":
		r.handleListHosts(w, req)
	case req.Method == http.MethodGet && path == "/xrpc/com.atproto.sync.getHostStatus":
		r.handleGetHostStatus(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *Relay) handleRequestCrawl(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Hostname string `json:"hostname"`
	}
	dec := json.NewDecoder(req.Body)
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	if body.Hostname == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing hostname")
		return
	}
	if err := r.RequestCrawl(req.Context(), body.Hostname); err != nil {
		switch {
		case errors.Is(err, ErrHostNotAllowed), errors.Is(err, ErrDIDNotAllowed):
			writeError(w, http.StatusForbidden, "Forbidden", err.Error())
		case errors.Is(err, ErrPDSNotFound), errors.Is(err, ErrInvalidHostname):
			writeError(w, http.StatusBadGateway, "PdsNotFound", "could not resolve PDS identity")
		default:
			writeError(w, http.StatusBadGateway, "PdsNotFound", "could not resolve PDS identity")
		}
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{})
}

func (r *Relay) handleSubscribeRepos(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query()
	var since *int64
	if raw := query.Get("cursor"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			since = &n
		}
	}
	filter, err := ParseSubscribeFilter(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(5_000_000)

	head := r.seq.Head()
	if since != nil && *since > head {
		msg := fmt.Sprintf("cursor %d is past the relay's newest event %d", *since, head)
		r.log.Warn("subscribe_repos_future_cursor", "cursor", *since, "head", head)
		_ = conn.WriteJSON(map[string]string{"error": FutureCursorError, "message": msg})
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, FutureCursorError), time.Now().Add(time.Second))
		return
	}

	var cursor any = "live"
	if since != nil {
		cursor = *since
	}
	if filter.Active() {
		r.log.Info("subscribe_repos_filter", "wantedCollections", filter.Collections, "wantedDidsCount", len(filter.DIDs),
			"wantedDids", events.LogDIDs(filter.DIDs), "cursor", cursor)
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteJSON(filter.info()); err != nil {
			return
		}
	}
	backlog, live, cancel := r.seq.Watch(since)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	defer func() {
		_ = conn.Close()
		<-done
	}()

	write := func(frame Frame) bool {
		frame, ok := filter.Apply(frame)
		if !ok {
			return true
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteJSON(frame); err != nil {
			return false
		}
		return true
	}
	for _, frame := range backlog {
		if !write(frame) {
			return
		}
	}
	ping := time.NewTicker(SubscribePingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		case <-done:
			return
		case <-r.ctx.Done():
			return
		case frame, ok := <-live:
			if !ok {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "internal error"), time.Now().Add(time.Second))
				return
			}
			if !write(frame) {
				return
			}
		}
	}
}

func (r *Relay) handleGetRepo(w http.ResponseWriter, req *http.Request) {
	did := req.URL.Query().Get("did")
	if did == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing did")
		return
	}
	account, ok := r.store.GetAccount(did)
	if !ok {
		writeError(w, http.StatusNotFound, "RepoNotFound", "unknown DID")
		return
	}
	target := "https://" + account.HostHostname + "/xrpc/com.atproto.sync.getRepo?did=" + url.QueryEscape(did)
	http.Redirect(w, req, target, http.StatusTemporaryRedirect)
}

func (r *Relay) handleGetRepoStatus(w http.ResponseWriter, req *http.Request) {
	did := req.URL.Query().Get("did")
	if did == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing did")
		return
	}
	account, ok := r.store.GetAccount(did)
	if !ok {
		writeError(w, http.StatusNotFound, "RepoNotFound", "unknown DID")
		return
	}
	out := map[string]any{
		"did":      account.DID,
		"hostname": account.HostHostname,
		"rev":      account.Rev,
		"seq":      account.Seq,
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Relay) handleListRepos(w http.ResponseWriter, req *http.Request) {
	limit := parseLimit(req.URL.Query().Get("limit"))
	cursor := req.URL.Query().Get("cursor")
	accounts := r.store.ListAccounts()
	start := 0
	if cursor != "" {
		start = len(accounts)
		for i, a := range accounts {
			if a.DID > cursor {
				start = i
				break
			}
		}
	}
	end := min(start+limit, len(accounts))
	page := accounts[start:end]
	repos := make([]map[string]string, 0, len(page))
	for _, a := range page {
		rev := ""
		if a.Rev != nil {
			rev = *a.Rev
		}
		repos = append(repos, map[string]string{"did": a.DID, "head": rev, "rev": rev})
	}
	out := map[string]any{"repos": repos}
	if len(page) == limit && len(page) > 0 {
		out["cursor"] = page[len(page)-1].DID
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Relay) handleListReposByCollection(w http.ResponseWriter, req *http.Request) {
	collection := req.URL.Query().Get("collection")
	if collection == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing collection")
		return
	}
	limit := parseLimit(req.URL.Query().Get("limit"))
	cursor := req.URL.Query().Get("cursor")
	dids, next := r.store.ListCollection(collection, cursor, limit)
	repos := make([]map[string]string, 0, len(dids))
	for _, did := range dids {
		repos = append(repos, map[string]string{"did": did})
	}
	out := map[string]any{"repos": repos}
	if next != "" {
		out["cursor"] = next
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Relay) handleGetLatestCommit(w http.ResponseWriter, req *http.Request) {
	did := req.URL.Query().Get("did")
	if did == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing did")
		return
	}
	account, ok := r.store.GetAccount(did)
	if !ok || account.Rev == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]string{"error": "RepoNotFound"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]string{"cid": *account.Rev, "rev": *account.Rev})
}

func (r *Relay) handleListHosts(w http.ResponseWriter, req *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{"hosts": r.store.ListHosts()})
}

func (r *Relay) handleGetHostStatus(w http.ResponseWriter, req *http.Request) {
	hostname := req.URL.Query().Get("hostname")
	if hostname == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing hostname")
		return
	}
	host, ok := r.store.GetHost(hostname)
	if !ok {
		httpjson.Write(w, http.StatusNotFound, map[string]string{"error": "HostNotFound"})
		return
	}
	httpjson.Write(w, http.StatusOK, host)
}

func (r *Relay) emit(ctx context.Context, frame Frame, evt *comatproto.SyncSubscribeRepos_Commit) {
	r.sinksMu.RLock()
	sinks := append([]EventSink{}, r.sinks...)
	r.sinksMu.RUnlock()
	if len(sinks) == 0 {
		return
	}
	ev := Event{Seq: frame.Seq, Frame: frame, Commit: evt}
	for _, sink := range sinks {
		if err := sink.HandleEvent(ctx, ev); err != nil {
			r.log.Warn("event sink failed", "err", err)
		}
	}
}

func (r *Relay) hostAllowed(hostname string) bool {
	if len(r.cfg.AllowedHosts) == 0 {
		return true
	}
	hostname = strings.ToLower(hostname)
	for _, h := range r.cfg.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "*" || h == hostname {
			return true
		}
		if suffix, ok := strings.CutPrefix(h, "*."); ok && strings.HasSuffix(hostname, "."+suffix) {
			return true
		}
	}
	return false
}

func (r *Relay) didAllowed(did string) bool {
	if len(r.cfg.AllowedDIDs) == 0 {
		return true
	}
	return slices.Contains(r.cfg.AllowedDIDs, did)
}

func parseLimit(raw string) int {
	limit := 50
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	return limit
}

func writeError(w http.ResponseWriter, status int, name, message string) {
	httpjson.Write(w, status, map[string]string{"error": name, "message": message})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

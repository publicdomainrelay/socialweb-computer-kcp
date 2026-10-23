package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/gorilla/websocket"
)

var (
	errNoSubscriber = errors.New("no active subscriber")
	errClosed       = errors.New("ingress: closed")
)

type Config struct {
	Hostname          string
	ServiceID         string
	RelayTimeout      time.Duration
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	ReconnectGrace    time.Duration
	NonceTTL          time.Duration
	AdditionalHosts   []string
	AllowedDIDs       []string
	AllowedHosts      []string
	Directory         identity.Directory
	PLCURL            string
	Logger            *slog.Logger
}

type Server struct {
	cfg    Config
	log    *slog.Logger
	dir    identity.Directory
	nonces *NonceStore

	mu            sync.Mutex
	peers         map[string]*peer
	subscriptions map[string]*activeSubscription
	pending       map[string]*pendingRequest
	keepalives    map[string]*keepalive
	closed        bool

	closedCh  chan struct{}
	closeOnce sync.Once
}

type peer struct {
	conn         *websocket.Conn
	did          string
	subdomain    string
	expiresAt    time.Time
	writeMu      sync.Mutex
	lastResponse time.Time
	closeOnce    sync.Once
	goneOnce     sync.Once
	gone         chan struct{}
	removed      bool
}

func (p *peer) writeText(data []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return p.conn.WriteMessage(websocket.TextMessage, data)
}

func (p *peer) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.writeText(b)
}

func (p *peer) close(code int, reason string) {
	p.closeOnce.Do(func() {
		_ = p.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
		_ = p.conn.Close()
	})
}

type activeSubscription struct {
	peer      *peer
	caller    *websocket.Conn
	callerMu  sync.Mutex
	subdomain string
	nsid      string
}

func (a *activeSubscription) writeText(data []byte) error {
	a.callerMu.Lock()
	defer a.callerMu.Unlock()
	_ = a.caller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return a.caller.WriteMessage(websocket.TextMessage, data)
}

func (a *activeSubscription) writeBinary(data []byte) error {
	a.callerMu.Lock()
	defer a.callerMu.Unlock()
	_ = a.caller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return a.caller.WriteMessage(websocket.BinaryMessage, data)
}

func (a *activeSubscription) closeCaller(code int, reason string) {
	_ = a.caller.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = a.caller.Close()
}

type dispatchResult struct {
	resp Response
	err  error
}

type pendingRequest struct {
	peer  *peer
	ch    chan dispatchResult
	timer *time.Timer
}

type keepalive struct {
	stop chan struct{}
	once sync.Once
}

func (k *keepalive) close() {
	k.once.Do(func() { close(k.stop) })
}

func NewServer(cfg Config) (*Server, error) {
	if cfg.Hostname == "" {
		return nil, errors.New("ingress: hostname is required")
	}
	if cfg.ServiceID == "" {
		cfg.ServiceID = DefaultServiceID
	}
	if cfg.RelayTimeout <= 0 {
		cfg.RelayTimeout = 30 * time.Second
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 5 * time.Second
	}
	if cfg.KeepaliveTimeout <= 0 {
		cfg.KeepaliveTimeout = 5 * time.Second
	}
	if cfg.NonceTTL <= 0 {
		cfg.NonceTTL = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	dir := cfg.Directory
	if dir == nil {
		dir = NewDirectory(cfg.PLCURL)
	}
	s := &Server{
		cfg:           cfg,
		log:           cfg.Logger,
		dir:           dir,
		nonces:        NewNonceStore(cfg.NonceTTL, dir),
		peers:         map[string]*peer{},
		subscriptions: map[string]*activeSubscription{},
		pending:       map[string]*pendingRequest{},
		keepalives:    map[string]*keepalive{},
		closedCh:      make(chan struct{}),
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.serveHTTP(w, req)
}

func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.closedCh) })
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	peers := make([]*peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	subs := make([]*activeSubscription, 0, len(s.subscriptions))
	for _, sub := range s.subscriptions {
		subs = append(subs, sub)
	}
	keepalives := make([]*keepalive, 0, len(s.keepalives))
	for _, k := range s.keepalives {
		keepalives = append(keepalives, k)
	}
	s.peers = map[string]*peer{}
	s.subscriptions = map[string]*activeSubscription{}
	s.keepalives = map[string]*keepalive{}
	for reqID, pr := range s.pending {
		delete(s.pending, reqID)
		pr.timer.Stop()
		select {
		case pr.ch <- dispatchResult{err: errClosed}:
		default:
		}
	}
	s.mu.Unlock()
	for _, k := range keepalives {
		k.close()
	}
	for _, p := range peers {
		p.close(1001, "server shutting down")
	}
	for _, sub := range subs {
		sub.closeCaller(1001, "server shutting down")
	}
}

func (s *Server) serveHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	host := hostnameOnly(hostFromRequest(req, s.cfg.Hostname))
	path := req.URL.Path
	control := s.isControlHost(host)

	if control && req.Method == http.MethodGet && path == "/.well-known/did.json" {
		writeJSON(w, http.StatusOK, map[string]any{
			"@context": []string{"https://www.w3.org/ns/did/v1"},
			"id":       hostnameToDID(host),
			"service": []map[string]string{{
				"id":              "#" + s.cfg.ServiceID,
				"type":            "XrpcRelay",
				"serviceEndpoint": "https://" + s.cfg.Hostname,
			}},
		})
		return
	}
	if control && path == "/xrpc/"+GetNonceNSID {
		s.handleNonce(w, req)
		return
	}
	if control && path == "/xrpc/"+SubscribeNSID {
		s.handleSubscribe(w, req)
		return
	}

	bases := s.dataBases()
	if _, subdomain, ok := splitDataHost(host, bases); ok {
		if websocket.IsWebSocketUpgrade(req) && strings.HasPrefix(path, "/xrpc/") {
			s.handleDataWS(w, req, subdomain)
			return
		}
		s.handleForward(w, req, subdomain)
		return
	}
	http.NotFound(w, req)
}

func (s *Server) isControlHost(host string) bool {
	if !s.hostInScope(host) {
		return false
	}
	if host == s.cfg.Hostname {
		return true
	}
	for _, h := range s.cfg.AdditionalHosts {
		if h == "*" || h == host {
			return true
		}
	}
	return false
}

func (s *Server) hostInScope(host string) bool {
	if len(s.cfg.AllowedHosts) == 0 {
		return true
	}
	for _, h := range s.cfg.AllowedHosts {
		if h == "*" || h == host {
			return true
		}
	}
	return false
}

func (s *Server) dataBases() []string {
	seen := map[string]struct{}{}
	out := []string{}
	add := func(h string) {
		h = hostnameOnly(h)
		if h == "" || h == "*" {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	if len(s.cfg.AllowedHosts) > 0 {
		for _, h := range s.cfg.AllowedHosts {
			add(h)
		}
		return out
	}
	add(s.cfg.Hostname)
	for _, h := range s.cfg.AdditionalHosts {
		add(h)
	}
	return out
}

func (s *Server) handleNonce(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.NotFound(w, req)
		return
	}
	token, err := BearerToken(req.Header.Get("Authorization"), "")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "AuthenticationRequired", "message": err.Error()})
		return
	}
	if _, err := VerifyServiceAuth(req.Context(), token, hostnameToDID(s.cfg.Hostname), GetNonceNSID, s.dir); err != nil {
		s.log.Warn("auth_denied", "nsid", GetNonceNSID, "err", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "AuthenticationRequired", "message": err.Error()})
		return
	}
	var input struct {
		Key        string      `json:"key"`
		Signatures []Signature `json:"signatures"`
	}
	_ = json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&input)
	if !strings.HasPrefix(input.Key, "did:") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "InvalidRequest", "message": "key must be a DID"})
		return
	}
	nonce := s.nonces.Issue(input.Key)
	s.log.Info("nonce_issued", "key", input.Key)
	writeJSON(w, http.StatusOK, map[string]any{"nonce": nonce, "signatures": []Signature{}})
}

func (s *Server) handleSubscribe(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.NotFound(w, req)
		return
	}
	authHeader := req.Header.Get("Authorization")
	token, err := BearerToken(authHeader, req.URL.Query().Get("service_auth"))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "AuthenticationRequired", "message": err.Error()})
		return
	}
	auth, err := VerifyServiceAuth(req.Context(), token, hostnameToDID(s.cfg.Hostname), SubscribeNSID, s.dir)
	if err != nil {
		s.log.Warn("auth_denied", "nsid", SubscribeNSID, "err", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "AuthenticationRequired", "message": err.Error()})
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(8_000_000)
	clientDID := req.URL.Query().Get("did")
	subdomain := didToSubdomain(clientDID)
	if !strings.HasPrefix(clientDID, "did:") {
		s.log.Warn("missing_did")
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "did query param must be a DID"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	var reg Registration
	if err := json.Unmarshal([]byte(req.URL.Query().Get("registration")), &reg); err != nil {
		s.log.Warn("registration_malformed", "did", clientDID)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "malformed registration"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	result := s.nonces.Verify(req.Context(), reg)
	if !result.OK {
		s.log.Warn("registration_rejected", "did", clientDID, "reason", result.Reason)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "registration rejected: "+result.Reason), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	if result.Key != clientDID {
		s.log.Warn("did_mismatch", "did", clientDID, "registration_key", result.Key)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "did does not match registration key"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	if !s.didAllowed(clientDID) {
		s.log.Warn("did_not_allowed", "did", clientDID)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "DID not in allowlist"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}

	p := &peer{conn: conn, did: clientDID, subdomain: subdomain, expiresAt: auth.ExpiresAt, gone: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		p.close(1001, "server shutting down")
		return
	}
	s.peers[subdomain] = p
	s.mu.Unlock()
	s.startKeepalive(p)
	go s.watchPeerExpiry(p)
	s.log.Info("subscriber_connected", "subdomain", subdomain, "key", result.Key)
	_ = p.writeJSON(map[string]any{
		"$type":      frameType("registered"),
		"subdomain":  subdomain,
		"ingressRef": "did:web:" + subdomain + "." + hostnameOnly(s.cfg.Hostname),
	})

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg frame
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		s.handlePeerFrame(p, msg)
	}
	s.dropPeer(p)
	s.log.Info("subscriber_disconnected", "subdomain", subdomain)
}

func (s *Server) handlePeerFrame(p *peer, msg frame) {
	switch kindOf(msg.Type) {
	case "response":
		s.handleResponse(p, msg)
	case "subscriptionOpen":
	case "subscriptionEvent":
		s.mu.Lock()
		sub := s.subscriptions[msg.SubscriptionID]
		s.mu.Unlock()
		if sub == nil || len(msg.Message) == 0 {
			return
		}
		if err := sub.writeText(msg.Message); err != nil {
			sub.closeCaller(1011, "subscription write failed")
		}
	case "subscriptionData":
		s.mu.Lock()
		sub := s.subscriptions[msg.SubscriptionID]
		s.mu.Unlock()
		if sub == nil {
			return
		}
		data, err := b64decode(msg.Data)
		if err != nil {
			return
		}
		if err := sub.writeBinary(data); err != nil {
			sub.closeCaller(1011, "subscription write failed")
		}
	case "subscriptionClose", "subscriptionCancel":
		s.mu.Lock()
		sub := s.subscriptions[msg.SubscriptionID]
		delete(s.subscriptions, msg.SubscriptionID)
		s.mu.Unlock()
		if sub == nil {
			return
		}
		code := msg.Code
		if code == 0 {
			code = 1000
		}
		sub.closeCaller(code, msg.Reason)
	}
}

func (s *Server) handleResponse(p *peer, msg frame) {
	s.mu.Lock()
	pr := s.pending[msg.RequestID]
	if pr != nil {
		delete(s.pending, msg.RequestID)
		p.lastResponse = time.Now()
	}
	s.mu.Unlock()
	if pr == nil {
		return
	}
	pr.timer.Stop()
	headers := map[string]string{}
	for k, v := range msg.Headers {
		if _, ok := relayedResponseHeaders[strings.ToLower(k)]; ok {
			headers[strings.ToLower(k)] = v
		}
	}
	pr.ch <- dispatchResult{resp: Response{
		Status:      msg.Status,
		Body:        msg.Body,
		ContentType: msg.ContentType,
		Headers:     headers,
	}}
}

func (p *peer) markGone() {
	if p.gone == nil {
		return
	}
	p.goneOnce.Do(func() { close(p.gone) })
}

func (s *Server) watchPeerExpiry(p *peer) {
	wait := time.Until(p.expiresAt)
	if wait <= 0 {
		s.log.Info("subscriber_expired", "subdomain", p.subdomain)
		s.evictPeer(p, 1008, "service auth token expired")
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		s.log.Info("subscriber_expired", "subdomain", p.subdomain)
		s.evictPeer(p, 1008, "service auth token expired")
	case <-p.gone:
	case <-s.closedCh:
	}
}

func (s *Server) dropPeer(p *peer) {
	p.markGone()
	s.mu.Lock()
	if p.removed {
		s.mu.Unlock()
		return
	}
	p.removed = true
	current := s.peers[p.subdomain] == p
	if current {
		delete(s.peers, p.subdomain)
	}
	var ka *keepalive
	if current {
		if cur, ok := s.keepalives[p.subdomain]; ok {
			ka = cur
			delete(s.keepalives, p.subdomain)
		}
	}
	for reqID, pr := range s.pending {
		if pr.peer != p {
			continue
		}
		delete(s.pending, reqID)
		pr.timer.Stop()
		select {
		case pr.ch <- dispatchResult{err: errNoSubscriber}:
		default:
		}
	}
	subs := make([]*activeSubscription, 0)
	for id, sub := range s.subscriptions {
		if sub.peer == p {
			delete(s.subscriptions, id)
			subs = append(subs, sub)
		}
	}
	s.mu.Unlock()
	if ka != nil {
		ka.close()
	}
	for _, sub := range subs {
		sub.closeCaller(4004, "subscriber disconnected")
	}
}

func (s *Server) evictPeer(p *peer, code int, reason string) {
	p.close(code, reason)
	s.dropPeer(p)
}

func (s *Server) startKeepalive(p *peer) {
	k := &keepalive{stop: make(chan struct{})}
	s.mu.Lock()
	if old := s.keepalives[p.subdomain]; old != nil {
		delete(s.keepalives, p.subdomain)
		old.close()
	}
	s.keepalives[p.subdomain] = k
	s.mu.Unlock()
	go func() {
		t := time.NewTicker(s.cfg.KeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-s.closedCh:
				return
			case <-t.C:
				s.mu.Lock()
				current := s.peers[p.subdomain]
				s.mu.Unlock()
				if current != p {
					return
				}
				f := map[string]any{
					"$type":     frameType("request"),
					"requestId": newID(),
					"method":    "GET",
					"path":      KeepalivePath,
					"params":    map[string]string{},
					"body":      nil,
					"headers":   map[string]string{},
				}
				reqID := f["requestId"].(string)
				go func() {
					_, _ = s.dispatch(context.Background(), p.subdomain, f, s.cfg.KeepaliveTimeout, reqID)
				}()
			}
		}
	}()
}

func (s *Server) dispatch(ctx context.Context, subdomain string, f map[string]any, timeout time.Duration, requestID string) (Response, error) {
	payload, err := json.Marshal(f)
	if err != nil {
		return Response{}, err
	}
	s.mu.Lock()
	p := s.peers[subdomain]
	if p == nil {
		s.mu.Unlock()
		return Response{}, fmt.Errorf("%w for subdomain %s", errNoSubscriber, subdomain)
	}
	ch := make(chan dispatchResult, 1)
	timer := time.NewTimer(timeout)
	pr := &pendingRequest{peer: p, ch: ch, timer: timer}
	s.pending[requestID] = pr
	s.mu.Unlock()

	if err := p.writeText(payload); err != nil {
		s.mu.Lock()
		if s.pending[requestID] == pr {
			delete(s.pending, requestID)
		}
		s.mu.Unlock()
		timer.Stop()
		return Response{}, err
	}

	select {
	case res := <-ch:
		return res.resp, res.err
	case <-timer.C:
		s.mu.Lock()
		if s.pending[requestID] == pr {
			delete(s.pending, requestID)
		}
		current := s.peers[subdomain]
		unresponsive := current == p && time.Since(p.lastResponse) >= timeout
		s.mu.Unlock()
		if unresponsive {
			s.evictPeer(p, 1011, fmt.Sprintf("subscriber unresponsive after %dms", timeout.Milliseconds()))
		}
		return Response{}, fmt.Errorf("relay timeout after %dms", timeout.Milliseconds())
	case <-ctx.Done():
		s.mu.Lock()
		if s.pending[requestID] == pr {
			delete(s.pending, requestID)
		}
		s.mu.Unlock()
		timer.Stop()
		return Response{}, ctx.Err()
	case <-s.closedCh:
		s.mu.Lock()
		if s.pending[requestID] == pr {
			delete(s.pending, requestID)
		}
		s.mu.Unlock()
		timer.Stop()
		return Response{}, errClosed
	}
}

func (s *Server) handleDataWS(w http.ResponseWriter, req *http.Request, subdomain string) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(8_000_000)
	path := req.URL.Path
	nsid := strings.TrimPrefix(path, "/xrpc/")
	params := lastQueryValues(req.URL.Query())

	s.mu.Lock()
	p := s.peers[subdomain]
	s.mu.Unlock()
	if p == nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4004, fmt.Sprintf("no active subscriber for subdomain %s", subdomain)), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}

	id := newID()
	sub := &activeSubscription{peer: p, caller: conn, subdomain: subdomain, nsid: nsid}
	s.mu.Lock()
	s.subscriptions[id] = sub
	s.mu.Unlock()
	_ = p.writeJSON(map[string]any{
		"$type":          frameType("subscribe"),
		"subscriptionId": id,
		"nsid":           nsid,
		"params":         params,
	})

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}
		if err := p.writeJSON(map[string]any{
			"$type":          frameType("subscriptionData"),
			"subscriptionId": id,
			"data":           b64encode(data),
		}); err != nil {
			break
		}
	}

	s.mu.Lock()
	if s.subscriptions[id] == sub {
		delete(s.subscriptions, id)
	}
	s.mu.Unlock()
	_ = p.writeJSON(map[string]any{
		"$type":          frameType("subscriptionCancel"),
		"subscriptionId": id,
		"reason":         "caller disconnected",
	})
	_ = conn.Close()
}

func (s *Server) handleForward(w http.ResponseWriter, req *http.Request, subdomain string) {
	var body any
	var rawBody *string
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		b, _ := io.ReadAll(io.LimitReader(req.Body, 8<<20))
		raw := string(b)
		rawBody = &raw
		var parsed any
		if err := json.Unmarshal(b, &parsed); err == nil {
			body = parsed
		}
	}
	headers := map[string]string{}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	if _, ok := headers["host"]; !ok {
		headers["host"] = req.Host
	}
	requestID := newID()
	f := map[string]any{
		"$type":     frameType("request"),
		"requestId": requestID,
		"method":    req.Method,
		"path":      req.URL.Path,
		"params":    lastQueryValues(req.URL.Query()),
		"headers":   headers,
	}
	if body != nil || rawBody != nil {
		f["body"] = body
	}
	if rawBody != nil {
		f["rawBody"] = *rawBody
	}

	result, err := s.dispatch(req.Context(), subdomain, f, s.cfg.RelayTimeout, requestID)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, errNoSubscriber.Error()) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "NotFound", "message": msg})
			return
		}
		s.log.Error("relay_failed", "path", req.URL.Path, "subdomain", subdomain, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "RelayError", "message": msg})
		return
	}

	contentType := result.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	for k, v := range result.Headers {
		w.Header().Set(k, v)
	}
	status := result.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(result.Body) > 0 {
		var str string
		if err := json.Unmarshal(result.Body, &str); err == nil {
			_, _ = w.Write([]byte(str))
			return
		}
		_, _ = w.Write(result.Body)
	}
}

func (s *Server) didAllowed(did string) bool {
	if len(s.cfg.AllowedDIDs) == 0 {
		return true
	}
	return slices.Contains(s.cfg.AllowedDIDs, did)
}

func lastQueryValues(values map[string][]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[len(v)-1]
		}
	}
	return out
}

func hostFromRequest(req *http.Request, fallback string) string {
	if req.Host != "" {
		return req.Host
	}
	if req.URL != nil && req.URL.Host != "" {
		return req.URL.Host
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

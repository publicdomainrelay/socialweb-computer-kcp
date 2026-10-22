package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Keypair interface {
	DID() string
	Sign(data []byte) ([]byte, error)
}

type Target struct {
	Hostname string
	Port     int
}

func (t Target) address() string {
	return net.JoinHostPort(t.Hostname, strconv.Itoa(t.Port))
}

type SubscriberConfig struct {
	Label                     string
	Keypair                   Keypair
	GetServiceAuthToken       func(ctx context.Context, nsid string) (string, error)
	IngressProxyHost          string
	TLS                       bool
	HandleRequest             func(ctx context.Context, req *Request) (*Response, error)
	TunnelTarget              *Target
	TunnelHandler             func(ctx context.Context, conn net.Conn) error
	WSTarget                  func() *Target
	DirectSubscriptionHandler func(subscriptionID, nsid string, params map[string]string, onEvent func(json.RawMessage), onData func([]byte)) (func(), error)
	Synthetic                 bool
	Dialer                    *websocket.Dialer
	HTTPClient                *http.Client
}

type Subscriber struct {
	cfg        SubscriberConfig
	did        string
	subdomain  string
	ingressRef string
	ctx        context.Context
	cancel     context.CancelFunc

	mu      sync.Mutex
	conn    *websocket.Conn
	closed  bool
	writeMu sync.Mutex

	tunnels        map[string]*tunnel
	wsSubs         map[string]*wsSubscription
	directSubs     map[string]func()
	syntheticStops map[string]func()
}

type tunnel struct {
	mu     sync.Mutex
	conn   net.Conn
	cancel context.CancelFunc
	queue  [][]byte
	closed bool
}

type wsSubscription struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	closed bool
}

func NewSubscriber(ctx context.Context, cfg SubscriberConfig) (*Subscriber, error) {
	if cfg.Keypair == nil {
		return nil, errors.New("ingress: subscriber keypair is required")
	}
	if cfg.IngressProxyHost == "" {
		return nil, errors.New("ingress: ingress proxy host is required")
	}
	if cfg.GetServiceAuthToken == nil {
		return nil, errors.New("ingress: GetServiceAuthToken is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	did := cfg.Keypair.DID()
	host := hostnameOnly(cfg.IngressProxyHost)
	s := &Subscriber{
		cfg:            cfg,
		did:            did,
		subdomain:      didToSubdomain(did),
		ingressRef:     "did:web:" + didToSubdomain(did) + "." + host,
		tunnels:        map[string]*tunnel{},
		wsSubs:         map[string]*wsSubscription{},
		directSubs:     map[string]func(){},
		syntheticStops: map[string]func(){},
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	if err := s.openOnce(s.ctx); err != nil {
		s.cancel()
		return nil, err
	}
	return s, nil
}

func (s *Subscriber) Subdomain() string  { return s.subdomain }
func (s *Subscriber) IngressRef() string { return s.ingressRef }
func (s *Subscriber) DID() string        { return s.did }

func (s *Subscriber) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	s.cancel()
	if conn != nil {
		_ = conn.Close()
	}
	s.cleanupSubscriptions()
}

func (s *Subscriber) openOnce(ctx context.Context) error {
	nonceToken, err := s.cfg.GetServiceAuthToken(ctx, GetNonceNSID)
	if err != nil {
		return err
	}
	nonceURL := httpOrigin(s.cfg.IngressProxyHost, s.cfg.TLS) + "/xrpc/" + GetNonceNSID
	body, _ := json.Marshal(map[string]any{"key": s.did, "signatures": []Signature{}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nonceURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+nonceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("nonce request failed: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var nonceResp struct {
		Nonce string `json:"nonce"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&nonceResp); err != nil {
		return err
	}
	nonceBytes, err := b64decode(nonceResp.Nonce)
	if err != nil {
		return err
	}
	sig, err := s.cfg.Keypair.Sign(nonceBytes)
	if err != nil {
		return err
	}
	subscribeToken, err := s.cfg.GetServiceAuthToken(ctx, SubscribeNSID)
	if err != nil {
		return err
	}
	reg, _ := json.Marshal(Registration{
		Type:  RegistrationType,
		Key:   s.did,
		Nonce: nonceResp.Nonce,
		Signatures: []Signature{{
			Key:       s.did,
			Signature: b64encode(sig),
		}},
	})
	wsURL := wsOrigin(s.cfg.IngressProxyHost, s.cfg.TLS) + "/xrpc/" + SubscribeNSID +
		"?registration=" + url.QueryEscape(string(reg)) +
		"&did=" + url.QueryEscape(s.did) +
		"&service_auth=" + url.QueryEscape(subscribeToken)

	dialer := s.cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(8_000_000)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	registered := false
	for !registered {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			_ = conn.Close()
			return err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg frame
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if kindOf(msg.Type) == "registered" {
			registered = true
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return errors.New("ingress: subscriber closed")
	}
	s.conn = conn
	s.mu.Unlock()
	go s.readLoop(conn)
	return nil
}

func (s *Subscriber) readLoop(conn *websocket.Conn) {
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
		s.handleFrame(msg)
	}
	_ = conn.Close()
	s.cleanupSubscriptions()
	s.mu.Lock()
	current := s.conn == conn
	if current {
		s.conn = nil
	}
	closed := s.closed
	s.mu.Unlock()
	if current && !closed {
		go s.reconnect(0)
	}
}

func (s *Subscriber) reconnect(attempt int) {
	if s.ctx.Err() != nil {
		return
	}
	if err := s.openOnce(s.ctx); err != nil {
		if s.ctx.Err() != nil {
			return
		}
		delay := time.Second
		for i := 0; i < attempt && delay < 30*time.Second; i++ {
			delay *= 2
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-s.ctx.Done():
		case <-t.C:
			s.reconnect(attempt + 1)
		}
	}
}

func (s *Subscriber) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return errors.New("ingress: subscriber is not connected")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, b)
}

func (s *Subscriber) handleFrame(msg frame) {
	switch kindOf(msg.Type) {
	case "request":
		s.handleRequest(msg)
	case "subscribe":
		s.handleSubscribe(msg)
	case "subscriptionData":
		data, err := b64decode(msg.Data)
		if err != nil {
			return
		}
		s.mu.Lock()
		t := s.tunnels[msg.SubscriptionID]
		s.mu.Unlock()
		if t != nil {
			t.write(data)
		}
	case "subscriptionCancel", "subscriptionClose":
		s.closeSubscription(msg.SubscriptionID)
	}
}

func (s *Subscriber) handleRequest(msg frame) {
	if s.cfg.HandleRequest == nil {
		return
	}
	params := msg.Params
	if params == nil {
		params = map[string]string{}
	}
	headers := msg.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	req := &Request{
		RequestID: msg.RequestID,
		Method:    msg.Method,
		Path:      msg.Path,
		Params:    params,
		Body:      msg.Body,
		Headers:   headers,
	}
	if msg.RawBody != nil {
		req.RawBody = *msg.RawBody
	}
	go func() {
		result, err := s.cfg.HandleRequest(s.ctx, req)
		if err != nil {
			_ = s.send(map[string]any{
				"$type":       frameType("response"),
				"requestId":   req.RequestID,
				"status":      http.StatusInternalServerError,
				"body":        map[string]string{"error": "InternalError", "message": err.Error()},
				"contentType": "application/json",
			})
			return
		}
		out := map[string]any{
			"$type":       frameType("response"),
			"requestId":   req.RequestID,
			"status":      result.Status,
			"body":        json.RawMessage(result.Body),
			"contentType": result.ContentType,
		}
		if len(result.Headers) > 0 {
			out["headers"] = result.Headers
		}
		_ = s.send(out)
	}()
}

func (s *Subscriber) handleSubscribe(msg frame) {
	params := msg.Params
	if params == nil {
		params = map[string]string{}
	}
	switch {
	case (s.cfg.TunnelTarget != nil || s.cfg.TunnelHandler != nil) && msg.NSID == TunnelNSID:
		s.startTunnel(msg.SubscriptionID)
	case s.cfg.WSTarget != nil && msg.NSID != TunnelNSID && msg.NSID != "":
		s.startWSSubscription(msg.SubscriptionID, msg.NSID, params)
	case s.cfg.DirectSubscriptionHandler != nil && msg.NSID != TunnelNSID && msg.NSID != "":
		id := msg.SubscriptionID
		unsub, err := s.cfg.DirectSubscriptionHandler(
			id,
			msg.NSID,
			params,
			func(event json.RawMessage) {
				_ = s.send(map[string]any{
					"$type":          frameType("subscriptionEvent"),
					"subscriptionId": id,
					"message":        event,
				})
			},
			func(data []byte) {
				_ = s.send(map[string]any{
					"$type":          frameType("subscriptionData"),
					"subscriptionId": id,
					"data":           b64encode(data),
				})
			},
		)
		if err != nil {
			_ = s.send(map[string]any{
				"$type":          frameType("subscriptionClose"),
				"subscriptionId": id,
				"code":           1011,
				"reason":         err.Error(),
			})
			return
		}
		if unsub != nil {
			s.mu.Lock()
			s.directSubs[id] = unsub
			s.mu.Unlock()
		}
		_ = s.send(map[string]any{
			"$type":          frameType("subscriptionOpen"),
			"subscriptionId": id,
		})
	case s.cfg.Synthetic:
		id := msg.SubscriptionID
		_ = s.send(map[string]any{
			"$type":          frameType("subscriptionOpen"),
			"subscriptionId": id,
		})
		stop := make(chan struct{})
		var once sync.Once
		s.mu.Lock()
		s.syntheticStops[id] = func() { once.Do(func() { close(stop) }) }
		s.mu.Unlock()
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			seq := int64(0)
			for {
				select {
				case <-stop:
					return
				case <-s.ctx.Done():
					return
				case <-t.C:
					event, _ := json.Marshal(map[string]any{
						"seq":   seq,
						"time":  time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
						"event": "synthetic",
					})
					seq++
					_ = s.send(map[string]any{
						"$type":          frameType("subscriptionEvent"),
						"subscriptionId": id,
						"message":        json.RawMessage(event),
					})
				}
			}
		}()
	}
}

func (s *Subscriber) startTunnel(id string) {
	t := &tunnel{}
	s.mu.Lock()
	s.tunnels[id] = t
	s.mu.Unlock()
	var conn net.Conn
	if s.cfg.TunnelHandler != nil {
		client, server := net.Pipe()
		conn = client
		ctx, cancel := context.WithCancel(s.ctx)
		t.mu.Lock()
		t.cancel = cancel
		t.mu.Unlock()
		go func() {
			defer cancel()
			defer server.Close()
			_ = s.cfg.TunnelHandler(ctx, server)
			_ = client.Close()
		}()
	} else {
		target := s.cfg.TunnelTarget
		dialed, err := net.Dial("tcp", target.address())
		if err != nil {
			s.mu.Lock()
			delete(s.tunnels, id)
			s.mu.Unlock()
			_ = s.send(map[string]any{
				"$type":          frameType("subscriptionClose"),
				"subscriptionId": id,
				"code":           1011,
				"reason":         err.Error(),
			})
			return
		}
		conn = dialed
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = conn.Close()
		return
	}
	t.conn = conn
	queue := t.queue
	t.queue = nil
	t.mu.Unlock()
	for _, chunk := range queue {
		if _, err := conn.Write(chunk); err != nil {
			_ = conn.Close()
			s.closeSubscription(id)
			return
		}
	}
	_ = s.send(map[string]any{
		"$type":          frameType("subscriptionOpen"),
		"subscriptionId": id,
	})
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_ = s.send(map[string]any{
					"$type":          frameType("subscriptionData"),
					"subscriptionId": id,
					"data":           b64encode(buf[:n]),
				})
			}
			if err != nil {
				break
			}
		}
		s.mu.Lock()
		delete(s.tunnels, id)
		s.mu.Unlock()
		t.markClosed()
		_ = conn.Close()
		_ = s.send(map[string]any{
			"$type":          frameType("subscriptionClose"),
			"subscriptionId": id,
			"code":           1000,
		})
	}()
}

func (t *tunnel) write(data []byte) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	conn := t.conn
	if conn == nil {
		t.queue = append(t.queue, append([]byte{}, data...))
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	_, _ = conn.Write(data)
}

func (t *tunnel) markClosed() {
	t.mu.Lock()
	t.closed = true
	cancel := t.cancel
	conn := t.conn
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Subscriber) startWSSubscription(id, nsid string, params map[string]string) {
	target := s.cfg.WSTarget()
	if target == nil {
		return
	}
	u := "ws://" + target.address() + "/xrpc/" + nsid
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	dialer := s.cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(s.ctx, u, nil)
	if err != nil {
		_ = s.send(map[string]any{
			"$type":          frameType("subscriptionClose"),
			"subscriptionId": id,
			"code":           1011,
			"reason":         err.Error(),
		})
		return
	}
	sub := &wsSubscription{conn: conn}
	s.mu.Lock()
	s.wsSubs[id] = sub
	s.mu.Unlock()
	_ = s.send(map[string]any{
		"$type":          frameType("subscriptionOpen"),
		"subscriptionId": id,
	})
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				code := 1000
				reason := ""
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					code = closeErr.Code
					reason = closeErr.Text
				}
				s.mu.Lock()
				delete(s.wsSubs, id)
				s.mu.Unlock()
				_ = s.send(map[string]any{
					"$type":          frameType("subscriptionClose"),
					"subscriptionId": id,
					"code":           code,
					"reason":         reason,
				})
				return
			}
			switch mt {
			case websocket.TextMessage:
				if !json.Valid(data) {
					continue
				}
				_ = s.send(map[string]any{
					"$type":          frameType("subscriptionEvent"),
					"subscriptionId": id,
					"message":        json.RawMessage(data),
				})
			case websocket.BinaryMessage:
				_ = s.send(map[string]any{
					"$type":          frameType("subscriptionData"),
					"subscriptionId": id,
					"data":           b64encode(data),
				})
			}
		}
	}()
}

func (s *Subscriber) closeSubscription(id string) {
	s.mu.Lock()
	t := s.tunnels[id]
	delete(s.tunnels, id)
	ws := s.wsSubs[id]
	delete(s.wsSubs, id)
	unsub := s.directSubs[id]
	delete(s.directSubs, id)
	stopSynthetic := s.syntheticStops[id]
	delete(s.syntheticStops, id)
	s.mu.Unlock()
	if t != nil {
		t.markClosed()
	}
	if ws != nil {
		ws.mu.Lock()
		ws.closed = true
		ws.mu.Unlock()
		_ = ws.conn.Close()
	}
	if unsub != nil {
		unsub()
	}
	if stopSynthetic != nil {
		stopSynthetic()
	}
}

func (s *Subscriber) cleanupSubscriptions() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.tunnels)+len(s.wsSubs)+len(s.directSubs)+len(s.syntheticStops))
	for id := range s.tunnels {
		ids = append(ids, id)
	}
	for id := range s.wsSubs {
		ids = append(ids, id)
	}
	for id := range s.directSubs {
		ids = append(ids, id)
	}
	for id := range s.syntheticStops {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.closeSubscription(id)
	}
}

type TunnelOptions struct {
	IngressProxyHost    string
	SubscriberSubdomain string
	NSID                string
	InsecureHTTP        bool
	Dialer              *websocket.Dialer
}

func TunnelOverRelay(ctx context.Context, opts TunnelOptions, readable io.Reader, writable io.Writer) error {
	nsid := opts.NSID
	if nsid == "" {
		nsid = TunnelNSID
	}
	host := opts.SubscriberSubdomain + "." + opts.IngressProxyHost
	scheme := "wss"
	if opts.InsecureHTTP {
		scheme = "ws"
	}
	u := scheme + "://" + host + "/xrpc/" + nsid
	dialer := opts.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(ctx, u, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			n, err := readable.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
				return
			}
		}
	}()
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == 1000 {
				return nil
			}
			select {
			case <-done:
				return nil
			default:
			}
			return err
		}
		switch mt {
		case websocket.BinaryMessage, websocket.TextMessage:
			if _, err := writable.Write(data); err != nil {
				return err
			}
		}
	}
}

type CallerConfig struct {
	IngressProxyHost    string
	SubscriberDID       string
	SubscriberSubdomain string
	NSID                string
	Cursor              *int64
	InsecureHTTP        bool
	OnEvent             func(event json.RawMessage)
	Dialer              *websocket.Dialer
}

type Caller struct {
	cfg    CallerConfig
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	conn   *websocket.Conn
	closed bool
}

func NewCaller(ctx context.Context, cfg CallerConfig) (*Caller, error) {
	subdomain := cfg.SubscriberSubdomain
	if subdomain == "" && cfg.SubscriberDID != "" {
		subdomain = didToSubdomain(cfg.SubscriberDID)
	}
	if subdomain == "" {
		return nil, errors.New("ingress: subscriber DID or subdomain required")
	}
	cfg.SubscriberSubdomain = subdomain
	if cfg.NSID == "" {
		cfg.NSID = "com.atproto.sync.subscribeRepos"
	}
	c := &Caller{cfg: cfg}
	c.ctx, c.cancel = context.WithCancel(ctx)
	if err := c.connect(); err != nil {
		c.cancel()
		return nil, err
	}
	return c, nil
}

func (c *Caller) connect() error {
	scheme := "wss"
	if c.cfg.InsecureHTTP {
		scheme = "ws"
	}
	u := scheme + "://" + c.cfg.SubscriberSubdomain + "." + c.cfg.IngressProxyHost + "/xrpc/" + c.cfg.NSID
	if c.cfg.Cursor != nil {
		u += "?cursor=" + strconv.FormatInt(*c.cfg.Cursor, 10)
	}
	dialer := c.cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(c.ctx, u, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	go c.readLoop(conn)
	return nil
}

func (c *Caller) readLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if c.cfg.OnEvent != nil {
			c.cfg.OnEvent(json.RawMessage(data))
		}
	}
	c.mu.Lock()
	current := c.conn == conn
	if current {
		c.conn = nil
	}
	closed := c.closed
	c.mu.Unlock()
	if !current || closed {
		return
	}
	go c.reconnect()
}

func (c *Caller) reconnect() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		if err := c.connect(); err == nil {
			return
		}
	}
}

func (c *Caller) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	c.cancel()
	if conn != nil {
		_ = conn.Close()
	}
}

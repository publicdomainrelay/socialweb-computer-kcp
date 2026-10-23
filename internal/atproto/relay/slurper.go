package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/gorilla/websocket"
	cbg "github.com/whyrusleeping/cbor-gen"
)

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

type hostSub struct {
	relay    *Relay
	hostname string
	did      string
	ctx      context.Context
	cancel   context.CancelFunc

	mu  sync.Mutex
	ws  *websocket.Conn
	seq atomic.Int64
}

func (s *hostSub) stop() {
	s.cancel()
	s.mu.Lock()
	if s.ws != nil {
		_ = s.ws.Close()
	}
	s.mu.Unlock()
}

func (s *hostSub) setConn(conn *websocket.Conn) {
	s.mu.Lock()
	s.ws = conn
	s.mu.Unlock()
}

func (r *Relay) RequestCrawl(ctx context.Context, hostname string) error {
	if !r.hostAllowed(hostname) {
		return fmt.Errorf("%w: %s", ErrHostNotAllowed, hostname)
	}
	did, err := r.resolvePDSIdentity(ctx, hostname)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPDSNotFound, err)
	}
	if !r.didAllowed(did) {
		return fmt.Errorf("%w: %s", ErrDIDNotAllowed, did)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	old := r.subs[hostname]
	delete(r.subs, hostname)
	r.mu.Unlock()
	if old != nil {
		old.stop()
	}

	subCtx, cancel := context.WithCancel(r.ctx)
	sub := &hostSub{relay: r, hostname: hostname, did: did, ctx: subCtx, cancel: cancel}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return ErrClosed
	}
	r.subs[hostname] = sub
	r.mu.Unlock()

	r.updateHost(hostname, func(h *HostInfo) {
		h.DID = did
		h.State = HostActive
		h.LastSeen = time.Now().UnixMilli()
	})
	r.log.Info("crawl_started", "hostname", hostname, "did", did)
	r.mu.Lock()
	if !r.closed {
		r.wg.Go(sub.run)
	}
	r.mu.Unlock()
	return nil
}

func (r *Relay) resolvePDSIdentity(ctx context.Context, hostname string) (string, error) {
	if hostname == "" {
		return "", ErrInvalidHostname
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	scheme := "https"
	if r.cfg.InsecureHTTP {
		scheme = "http"
	}
	u := scheme + "://" + hostname + "/xrpc/com.atproto.server.describeServer"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("describeServer returned %d", resp.StatusCode)
	}
	var body struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if !strings.HasPrefix(body.DID, "did:") {
		return "", fmt.Errorf("missing did in describeServer response")
	}
	return body.DID, nil
}

func (r *Relay) updateHost(hostname string, f func(*HostInfo)) {
	r.hostMu.Lock()
	defer r.hostMu.Unlock()
	h, ok := r.store.GetHost(hostname)
	if !ok {
		h = HostInfo{Hostname: hostname, State: HostConnecting}
	}
	f(&h)
	h.Hostname = hostname
	r.store.PutHost(h)
}

func (s *hostSub) run() {
	var cursor int64
	if account, ok := s.relay.store.GetAccount(s.did); ok && account.Seq != nil {
		cursor = *account.Seq
	}
	attempt := 0
	for {
		if s.ctx.Err() != nil {
			return
		}
		conn, err := s.dial(cursor)
		if err != nil {
			s.relay.log.Warn("pds_connect_failed", "hostname", s.hostname, "err", err)
			s.recordRetry()
			attempt++
			s.sleep(attempt)
			continue
		}
		s.setConn(conn)
		now := time.Now().UnixMilli()
		s.relay.updateHost(s.hostname, func(h *HostInfo) {
			h.DID = s.did
			h.State = HostActive
			h.LastSeen = now
			h.RetryCount = 0
		})
		s.relay.log.Info("pds_connected", "hostname", s.hostname)
		err = s.readLoop(conn)
		_ = conn.Close()
		s.setConn(nil)
		if s.ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrFutureCursor) {
			s.relay.updateHost(s.hostname, func(h *HostInfo) { h.State = HostIdle })
			return
		}
		s.relay.log.Info("pds_disconnected", "hostname", s.hostname, "err", err)
		updated := s.seq.Load()
		if updated > cursor {
			cursor = updated
			attempt = 0
		}
		attempt++
		s.sleep(attempt)
	}
}

func (s *hostSub) recordRetry() {
	s.relay.updateHost(s.hostname, func(h *HostInfo) {
		h.RetryCount++
		h.State = HostConnecting
		h.LastSeen = time.Now().UnixMilli()
	})
}

func (s *hostSub) sleep(attempt int) {
	delay := time.Second
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
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
	}
}

func (s *hostSub) dial(cursor int64) (*websocket.Conn, error) {
	scheme := "wss"
	if s.relay.cfg.InsecureHTTP {
		scheme = "ws"
	}
	u := scheme + "://" + s.hostname + "/xrpc/com.atproto.sync.subscribeRepos"
	u += "?cursor=" + strconv.FormatInt(cursor, 10)
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second, TLSClientConfig: s.relay.cfg.TLSConfig, NetDialContext: s.relay.cfg.DialContext}
	header := http.Header{}
	header.Set("User-Agent", "socialweb-computer relay")
	conn, resp, err := d.DialContext(s.ctx, u, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, err
	}
	return conn, nil
}

func (s *hostSub) readLoop(conn *websocket.Conn) error {
	conn.SetReadLimit(5_000_000)
	stopPing := make(chan struct{})
	var ping sync.WaitGroup
	defer ping.Wait()
	ping.Go(func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-s.ctx.Done():
				_ = conn.Close()
				return
			case <-t.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			}
		}
	})
	defer close(stopPing)

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		evt, err := parseStreamMessage(mt, data)
		if err != nil {
			s.relay.log.Warn("pds_frame_invalid", "hostname", s.hostname, "err", err)
			continue
		}
		if evt == nil {
			continue
		}
		if err := s.handleEvent(evt); err != nil {
			return err
		}
	}
}

func (s *hostSub) observeSeq(seq int64) {
	if seq <= 0 {
		return
	}
	for {
		current := s.seq.Load()
		if seq <= current || s.seq.CompareAndSwap(current, seq) {
			return
		}
	}
}

func (s *hostSub) handleEvent(evt *streamEvent) error {
	switch {
	case evt.errFrame != nil:
		if evt.errFrame.Error == FutureCursorError {
			return ErrFutureCursor
		}
		s.relay.log.Warn("pds_error_frame", "hostname", s.hostname, "error", evt.errFrame.Error, "message", evt.errFrame.Message)
		return nil
	case evt.commit != nil:
		if evt.commit.Seq <= s.seq.Load() {
			return nil
		}
		s.observeSeq(evt.commit.Seq)
		if !s.relay.didAllowed(evt.commit.Repo) {
			return nil
		}
		s.processCommit(evt.commit)
	case evt.sync != nil:
		if evt.sync.Seq <= s.seq.Load() {
			return nil
		}
		s.observeSeq(evt.sync.Seq)
	case evt.identity != nil:
		s.observeSeq(evt.identity.Seq)
	case evt.account != nil:
		s.observeSeq(evt.account.Seq)
	}
	return nil
}

func (s *hostSub) processCommit(evt *comatproto.SyncSubscribeRepos_Commit) {
	r := s.relay
	prev, ok := r.store.GetAccount(evt.Repo)
	var prevPtr *AccountInfo
	if ok {
		p := prev
		prevPtr = &p
	}
	if !r.cfg.DisableVerify {
		if err := r.verify.verify(s.ctx, evt, prevPtr, s.hostname); err != nil {
			if r.cfg.StrictVerify {
				r.log.Warn("commit_rejected", "hostname", s.hostname, "did", evt.Repo, "rev", evt.Rev, "err", err)
				return
			}
			r.log.Warn("commit_verification_failed", "hostname", s.hostname, "did", evt.Repo, "rev", evt.Rev, "err", err)
		}
	}

	rev := evt.Rev
	seq := evt.Seq
	r.store.PutAccount(AccountInfo{
		DID:          evt.Repo,
		HostHostname: s.hostname,
		Rev:          &rev,
		Seq:          &seq,
	})
	for _, op := range evt.Ops {
		if collection, ok := splitRecordPath(op.Path); ok {
			r.store.AddCollection(collection, evt.Repo)
		}
	}
	frame := r.seq.Append(frameFromCommit(evt))
	now := time.Now().UnixMilli()
	r.updateHost(s.hostname, func(h *HostInfo) {
		h.DID = s.did
		h.State = HostActive
		h.LastSeen = now
		h.Cursor = &seq
	})
	r.emit(s.ctx, frame, evt)
}

type errorFrame struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type streamEvent struct {
	kind     string
	commit   *comatproto.SyncSubscribeRepos_Commit
	sync     *comatproto.SyncSubscribeRepos_Sync
	identity *comatproto.SyncSubscribeRepos_Identity
	account  *comatproto.SyncSubscribeRepos_Account
	info     *comatproto.SyncSubscribeRepos_Info
	errFrame *errorFrame
}

func parseStreamMessage(mt int, data []byte) (*streamEvent, error) {
	if mt == websocket.BinaryMessage {
		return parseCBOREvent(data)
	}
	if mt != websocket.TextMessage {
		return nil, nil
	}
	return parseJSONEvent(data)
}

func parseJSONEvent(data []byte) (*streamEvent, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var probe struct {
		Type    string          `json:"$type"`
		Message json.RawMessage `json:"message"`
		Seq     *int64          `json:"seq"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	if strings.Contains(probe.Type, "#subscriptionEvent") && len(probe.Message) > 0 {
		data = probe.Message
		if err := json.Unmarshal(data, &probe); err != nil {
			return nil, err
		}
	}
	kind := probe.Type
	if idx := strings.Index(kind, "#"); idx != -1 {
		kind = kind[idx:]
	}
	if kind == "" {
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		_, hasRepo := raw["repo"]
		_, hasActive := raw["active"]
		_, hasDid := raw["did"]
		_, hasHandle := raw["handle"]
		_, hasName := raw["name"]
		_, hasError := raw["error"]
		switch {
		case hasRepo:
			kind = "#commit"
		case hasActive:
			kind = "#account"
		case hasDid && hasHandle:
			kind = "#identity"
		case hasName:
			kind = "#info"
		case hasError:
			kind = "#error"
		}
	}
	normalized, err := normalizeJSONBlocks(data)
	if err != nil {
		return nil, err
	}
	evt := &streamEvent{kind: kind}
	switch kind {
	case "#commit":
		var commit comatproto.SyncSubscribeRepos_Commit
		if err := json.Unmarshal(normalized, &commit); err != nil {
			return nil, err
		}
		evt.commit = &commit
	case "#sync":
		var syncEvt comatproto.SyncSubscribeRepos_Sync
		if err := json.Unmarshal(normalized, &syncEvt); err != nil {
			return nil, err
		}
		evt.sync = &syncEvt
	case "#identity":
		var ident comatproto.SyncSubscribeRepos_Identity
		if err := json.Unmarshal(normalized, &ident); err != nil {
			return nil, err
		}
		evt.identity = &ident
	case "#account":
		var account comatproto.SyncSubscribeRepos_Account
		if err := json.Unmarshal(normalized, &account); err != nil {
			return nil, err
		}
		evt.account = &account
	case "#info":
		var info comatproto.SyncSubscribeRepos_Info
		if err := json.Unmarshal(normalized, &info); err != nil {
			return nil, err
		}
		evt.info = &info
	case "#error":
		var ef errorFrame
		if err := json.Unmarshal(normalized, &ef); err != nil {
			return nil, err
		}
		evt.errFrame = &ef
	default:
		return nil, nil
	}
	return evt, nil
}

func normalizeJSONBlocks(data []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	v, ok := raw["blocks"]
	if !ok || v == nil {
		delete(raw, "blocks")
		return json.Marshal(raw)
	}
	switch b := v.(type) {
	case string:
		raw["blocks"] = map[string]any{"$bytes": b}
	case map[string]any:
		if _, ok := b["$bytes"]; ok {
			return json.Marshal(raw)
		}
		keys := make([]int, 0, len(b))
		for k := range b {
			n, err := strconv.Atoi(k)
			if err != nil || n < 0 {
				keys = nil
				break
			}
			keys = append(keys, n)
		}
		if len(keys) == 0 {
			delete(raw, "blocks")
			return json.Marshal(raw)
		}
		sort.Ints(keys)
		buf := make([]byte, 0, len(keys))
		for _, k := range keys {
			val, _ := b[strconv.Itoa(k)].(float64)
			buf = append(buf, byte(val))
		}
		raw["blocks"] = map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(buf)}
	default:
		delete(raw, "blocks")
	}
	return json.Marshal(raw)
}

func parseCBOREvent(data []byte) (*streamEvent, error) {
	cr := cbg.NewCborReader(bytes.NewReader(data))
	op, msgType, err := readEventHeader(cr)
	if err != nil {
		return nil, err
	}
	if op == -1 {
		ef := &errorFrame{}
		if err := readErrorFrame(cr, ef); err != nil {
			return nil, err
		}
		return &streamEvent{kind: "#error", errFrame: ef}, nil
	}
	if op != 1 {
		return nil, fmt.Errorf("unrecognized event stream frame type: %d", op)
	}
	switch msgType {
	case "#commit":
		var evt comatproto.SyncSubscribeRepos_Commit
		if err := evt.UnmarshalCBOR(cr); err != nil {
			return nil, err
		}
		return &streamEvent{kind: msgType, commit: &evt}, nil
	case "#sync":
		var evt comatproto.SyncSubscribeRepos_Sync
		if err := evt.UnmarshalCBOR(cr); err != nil {
			return nil, err
		}
		return &streamEvent{kind: msgType, sync: &evt}, nil
	case "#identity":
		var evt comatproto.SyncSubscribeRepos_Identity
		if err := evt.UnmarshalCBOR(cr); err != nil {
			return nil, err
		}
		return &streamEvent{kind: msgType, identity: &evt}, nil
	case "#account":
		var evt comatproto.SyncSubscribeRepos_Account
		if err := evt.UnmarshalCBOR(cr); err != nil {
			return nil, err
		}
		return &streamEvent{kind: msgType, account: &evt}, nil
	case "#info":
		var evt comatproto.SyncSubscribeRepos_Info
		if err := evt.UnmarshalCBOR(cr); err != nil {
			return nil, err
		}
		return &streamEvent{kind: msgType, info: &evt}, nil
	default:
		return nil, nil
	}
}

func readEventHeader(cr *cbg.CborReader) (int64, string, error) {
	major, n, err := cr.ReadHeader()
	if err != nil {
		return 0, "", err
	}
	if major != 5 {
		return 0, "", fmt.Errorf("event header is not a map")
	}
	var op int64
	var msgType string
	for range n {
		key, err := readCBORString(cr)
		if err != nil {
			return 0, "", err
		}
		switch key {
		case "op":
			major, v, err := cr.ReadHeader()
			if err != nil {
				return 0, "", err
			}
			switch major {
			case 0:
				op = int64(v)
			case 1:
				op = -1 - int64(v)
			default:
				return 0, "", fmt.Errorf("event header op is not an integer")
			}
		case "t":
			s, err := readCBORString(cr)
			if err != nil {
				return 0, "", err
			}
			msgType = s
		default:
			if err := skipCBORValue(cr); err != nil {
				return 0, "", err
			}
		}
	}
	return op, msgType, nil
}

func readErrorFrame(cr *cbg.CborReader, ef *errorFrame) error {
	major, n, err := cr.ReadHeader()
	if err != nil {
		return err
	}
	if major != 5 {
		return fmt.Errorf("error frame is not a map")
	}
	for range n {
		key, err := readCBORString(cr)
		if err != nil {
			return err
		}
		if key == "error" || key == "message" {
			val, err := readCBORString(cr)
			if err != nil {
				return err
			}
			if key == "error" {
				ef.Error = val
			} else {
				ef.Message = val
			}
			continue
		}
		if err := skipCBORValue(cr); err != nil {
			return err
		}
	}
	return nil
}

func readCBORString(cr *cbg.CborReader) (string, error) {
	major, n, err := cr.ReadHeader()
	if err != nil {
		return "", err
	}
	if major != 3 {
		return "", fmt.Errorf("expected CBOR text string")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(cr, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func skipCBORValue(cr *cbg.CborReader) error {
	major, n, err := cr.ReadHeader()
	if err != nil {
		return err
	}
	switch major {
	case 0, 1, 7:
		return nil
	case 2, 3:
		_, err := io.CopyN(io.Discard, cr, int64(n))
		return err
	case 4:
		for range n {
			if err := skipCBORValue(cr); err != nil {
				return err
			}
		}
		return nil
	case 5:
		for i := uint64(0); i < n*2; i++ {
			if err := skipCBORValue(cr); err != nil {
				return err
			}
		}
		return nil
	case 6:
		return skipCBORValue(cr)
	default:
		return fmt.Errorf("unsupported CBOR major type %d", major)
	}
}

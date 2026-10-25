package pdsclient

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/wsconn"
)

const (
	initialReconnectDelay = 500 * time.Millisecond
	maxReconnectDelay     = 15 * time.Second
	maxReconnectAttempts  = 12
	localHandshakeTimeout = 10 * time.Second
)

var repoFrameDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{DefaultMapType: reflect.TypeFor[map[string]any]()}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func newLocalSubscribe(ctx context.Context, handlers Handlers, base *url.URL, cursor *int64) (RepoEventStream, error) {
	if handlers == nil {
		return nil, fmt.Errorf("%w: local handler registry is required", ErrInvalidConfig)
	}
	return openWebSocketStream(ctx, subscribeURL(base, cursor), wsconn.Config{
		HandshakeTimeout: localHandshakeTimeout,
		Dial: func(ctx context.Context, _, address string) (net.Conn, error) {
			return handlers.DialHTTP(ctx, address)
		},
	})
}

func newRemoteSubscribe(ctx context.Context, base *url.URL, tlsConfig *tls.Config, cursor *int64) (RepoEventStream, error) {
	return openWebSocketStream(ctx, subscribeURL(base, cursor), wsconn.Config{TLSConfig: tlsConfig})
}

func subscribeURL(base *url.URL, cursor *int64) string {
	out := *base
	switch strings.ToLower(out.Scheme) {
	case "https":
		out.Scheme = "wss"
	default:
		out.Scheme = "ws"
	}
	out.Path = strings.TrimRight(out.Path, "/") + "/xrpc/com.atproto.sync.subscribeRepos"
	out.RawPath = ""
	out.Fragment = ""
	out.RawQuery = ""
	if cursor != nil {
		query := url.Values{}
		query.Set("cursor", strconv.FormatInt(*cursor, 10))
		out.RawQuery = query.Encode()
	}
	return out.String()
}

func openWebSocketStream(ctx context.Context, rawURL string, cfg wsconn.Config) (RepoEventStream, error) {
	conn, err := wsconn.DialConfig(ctx, rawURL, cfg)
	if err != nil {
		var handshake *wsconn.HandshakeError
		if errors.As(err, &handshake) {
			message := cmp.Or(http.StatusText(handshake.StatusCode), "websocket handshake failed")
			return nil, &XRPCError{StatusCode: handshake.StatusCode, Code: "WebSocketHandshake", Message: message}
		}
		return nil, fmt.Errorf("pdsclient: %w", err)
	}
	return &wsEventStream{conn: conn}, nil
}

type wsEventStream struct {
	conn   *wsconn.Conn
	cursor atomic.Int64
}

func (s *wsEventStream) Next(ctx context.Context) (RepoEvent, error) {
	for {
		data, err := s.conn.Read(ctx)
		if err != nil {
			return RepoEvent{}, err
		}
		event, ok, err := decodeRepoFrame(data)
		if err != nil {
			return RepoEvent{}, err
		}
		if !ok {
			continue
		}
		if event.Seq >= 0 {
			s.cursor.Store(event.Seq + 1)
		}
		return event, nil
	}
}

func (s *wsEventStream) Cursor() int64 {
	value := s.cursor.Load()
	if value == 0 {
		return -1
	}
	return value - 1
}

func (s *wsEventStream) Close() error {
	return s.conn.Close()
}

type repoEventStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	open   func(context.Context, *int64) (RepoEventStream, error)

	mu       sync.Mutex
	stream   RepoEventStream
	cursor   int64
	closed   bool
	delay    time.Duration
	failures int
}

func newRepoEventStream(parent context.Context, cursor *int64, open func(context.Context, *int64) (RepoEventStream, error)) (RepoEventStream, error) {
	if open == nil {
		return nil, fmt.Errorf("%w: subscription dial is not configured", ErrInvalidConfig)
	}
	ctx, cancel := context.WithCancel(parent)
	initial := int64(-1)
	if cursor != nil {
		initial = *cursor
	}
	first, err := open(ctx, cursor)
	if err != nil {
		cancel()
		return nil, err
	}
	return &repoEventStream{
		ctx:    ctx,
		cancel: cancel,
		open:   open,
		stream: first,
		cursor: initial,
		delay:  initialReconnectDelay,
	}, nil
}

func (s *repoEventStream) Next(ctx context.Context) (RepoEvent, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return RepoEvent{}, ErrClosed
		}
		stream := s.stream
		s.mu.Unlock()

		if stream == nil {
			if err := s.ctx.Err(); err != nil {
				return RepoEvent{}, err
			}
			s.mu.Lock()
			var cursor *int64
			if s.cursor >= 0 {
				value := s.cursor
				cursor = &value
			}
			s.mu.Unlock()
			dialCtx, cancel := context.WithCancel(s.ctx)
			stop := context.AfterFunc(ctx, cancel)
			stream, err := s.open(dialCtx, cursor)
			stop()
			cancel()
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return RepoEvent{}, ctxErr
				}
				if s.ctx.Err() != nil {
					return RepoEvent{}, s.ctx.Err()
				}
				if permanentStreamError(err) {
					return RepoEvent{}, err
				}
				if s.failures+1 > maxReconnectAttempts {
					return RepoEvent{}, err
				}
				s.failures++
				if waitErr := waitContext(ctx, s.delay); waitErr != nil {
					return RepoEvent{}, waitErr
				}
				s.delay = nextReconnectDelay(s.delay)
				continue
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = stream.Close()
				return RepoEvent{}, ErrClosed
			}
			s.stream = stream
			s.mu.Unlock()
			continue
		}

		event, err := stream.Next(ctx)
		if err == nil {
			s.mu.Lock()
			if event.Seq >= 0 {
				s.cursor = event.Seq
			}
			s.failures = 0
			s.delay = initialReconnectDelay
			s.mu.Unlock()
			return event, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RepoEvent{}, ctxErr
		}
		_ = stream.Close()
		s.mu.Lock()
		if s.stream == stream {
			s.stream = nil
		}
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return RepoEvent{}, s.ctx.Err()
		}
		if permanentStreamError(err) {
			return RepoEvent{}, err
		}
		if s.failures+1 > maxReconnectAttempts {
			return RepoEvent{}, err
		}
		s.failures++
		if waitErr := waitContext(ctx, s.delay); waitErr != nil {
			return RepoEvent{}, waitErr
		}
		s.delay = nextReconnectDelay(s.delay)
	}
}

func (s *repoEventStream) Cursor() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *repoEventStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream := s.stream
	s.stream = nil
	s.mu.Unlock()
	s.cancel()
	if stream != nil {
		return stream.Close()
	}
	return nil
}

func nextReconnectDelay(current time.Duration) time.Duration {
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func permanentStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrProtocol) || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrClosed) {
		return true
	}
	var xrpcErr *XRPCError
	if errors.As(err, &xrpcErr) {
		if xrpcErr.StatusCode == 0 {
			return true
		}
		if xrpcErr.StatusCode >= 400 && xrpcErr.StatusCode < 500 {
			return xrpcErr.StatusCode != http.StatusRequestTimeout && xrpcErr.StatusCode != http.StatusTooManyRequests
		}
	}
	return false
}

type frameHeader struct {
	Op int64  `cbor:"op"`
	T  string `cbor:"t"`
}

func DecodeRepoFrame(data []byte) (RepoEvent, bool, error) {
	return decodeRepoFrame(data)
}

func decodeRepoFrame(data []byte) (RepoEvent, bool, error) {
	var header frameHeader
	rest, err := repoFrameDecMode.UnmarshalFirst(data, &header)
	if err != nil {
		return RepoEvent{}, false, fmt.Errorf("%w: decoding frame header: %v", ErrProtocol, err)
	}
	if header.Op < 0 {
		var body struct {
			Error   string `cbor:"error"`
			Message string `cbor:"message"`
		}
		_ = cbor.Unmarshal(rest, &body)
		code := strings.TrimSpace(body.Error)
		message := strings.TrimSpace(body.Message)
		code = cmp.Or(code, "StreamError")
		message = cmp.Or(message, "stream error")
		return RepoEvent{}, false, &XRPCError{Code: code, Message: message}
	}
	if header.T != "" && header.T != "#commit" {
		return RepoEvent{}, false, nil
	}
	var body map[string]any
	if err := repoFrameDecMode.Unmarshal(rest, &body); err != nil {
		return RepoEvent{}, false, fmt.Errorf("%w: decoding commit frame: %v", ErrProtocol, err)
	}
	event, err := commitFromMap(body)
	if err != nil {
		return RepoEvent{}, false, err
	}
	return event, true, nil
}

func commitFromMap(body map[string]any) (RepoEvent, error) {
	var event RepoEvent
	repoRaw, err := requiredString(body["repo"])
	if err != nil {
		return RepoEvent{}, err
	}
	repo, err := syntax.ParseDID(repoRaw)
	if err != nil {
		return RepoEvent{}, fmt.Errorf("%w: invalid repo did", ErrProtocol)
	}
	event.Repo = repo
	seq, ok := int64Value(body["seq"])
	if !ok || seq < 0 {
		return RepoEvent{}, fmt.Errorf("%w: invalid seq", ErrProtocol)
	}
	event.Seq = seq
	commit, err := cidValue(body["commit"])
	if err != nil {
		return RepoEvent{}, err
	}
	if commit == nil {
		return RepoEvent{}, fmt.Errorf("%w: missing commit cid", ErrProtocol)
	}
	event.Commit = *commit
	revRaw, err := requiredString(body["rev"])
	if err != nil {
		return RepoEvent{}, err
	}
	rev, err := syntax.ParseTID(revRaw)
	if err != nil {
		return RepoEvent{}, fmt.Errorf("%w: invalid rev", ErrProtocol)
	}
	event.Rev = rev
	since, err := optionalString(body["since"])
	if err != nil {
		return RepoEvent{}, err
	}
	if since != nil {
		parsed, err := syntax.ParseTID(*since)
		if err != nil {
			return RepoEvent{}, fmt.Errorf("%w: invalid since", ErrProtocol)
		}
		event.Since = &parsed
	}
	prevData, err := cidValue(body["prevData"])
	if err != nil {
		return RepoEvent{}, err
	}
	event.PrevData = prevData
	if blocks, ok := body["blocks"].([]byte); ok {
		event.Blocks = append([]byte(nil), blocks...)
	}
	ops, err := repoOpsValue(body["ops"])
	if err != nil {
		return RepoEvent{}, err
	}
	event.Ops = ops
	blobs, err := cidListValue(body["blobs"])
	if err != nil {
		return RepoEvent{}, err
	}
	event.Blobs = blobs
	if rawTime, ok := body["time"].(string); ok && rawTime != "" {
		parsed, err := time.Parse(time.RFC3339Nano, rawTime)
		if err != nil {
			return RepoEvent{}, fmt.Errorf("%w: invalid event time", ErrProtocol)
		}
		event.Time = parsed
	}
	if tooBig, ok := body["tooBig"].(bool); ok {
		event.TooBig = tooBig
	}
	return event, nil
}

func requiredString(value any) (string, error) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%w: missing string field", ErrProtocol)
	}
	return raw, nil
}

func optionalString(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	raw, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%w: invalid string field", ErrProtocol)
	}
	if raw == "" {
		return nil, nil
	}
	return &raw, nil
}

func int64Value(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, true
	case uint64:
		if number <= math.MaxInt64 {
			return int64(number), true
		}
	case int:
		return int64(number), true
	case uint:
		if uint64(number) <= math.MaxInt64 {
			return int64(number), true
		}
	case uint32:
		return int64(number), true
	case int32:
		return int64(number), true
	}
	return 0, false
}

func cidValue(value any) (*cid.Cid, error) {
	if value == nil {
		return nil, nil
	}
	tag, ok := value.(cbor.Tag)
	if !ok {
		return nil, fmt.Errorf("%w: invalid cid field", ErrProtocol)
	}
	if tag.Number != 42 {
		return nil, fmt.Errorf("%w: unexpected cid tag", ErrProtocol)
	}
	raw, ok := tag.Content.([]byte)
	if !ok || len(raw) < 2 || raw[0] != 0 {
		return nil, fmt.Errorf("%w: invalid cid bytes", ErrProtocol)
	}
	parsed, err := cid.Cast(raw[1:])
	if err != nil || !parsed.Defined() {
		return nil, fmt.Errorf("%w: invalid cid", ErrProtocol)
	}
	return &parsed, nil
}

func repoOpsValue(value any) ([]RepoOp, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: invalid ops", ErrProtocol)
	}
	out := make([]RepoOp, 0, len(list))
	for _, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: invalid repo op", ErrProtocol)
		}
		action, err := requiredString(fields["action"])
		if err != nil {
			return nil, err
		}
		path, err := requiredString(fields["path"])
		if err != nil {
			return nil, err
		}
		opCID, err := cidValue(fields["cid"])
		if err != nil {
			return nil, err
		}
		prev, err := cidValue(fields["prev"])
		if err != nil {
			return nil, err
		}
		out = append(out, RepoOp{Action: action, Path: path, CID: opCID, Prev: prev})
	}
	return out, nil
}

func cidListValue(value any) ([]cid.Cid, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: invalid blobs", ErrProtocol)
	}
	out := make([]cid.Cid, 0, len(list))
	for _, item := range list {
		parsed, err := cidValue(item)
		if err != nil {
			return nil, err
		}
		if parsed == nil {
			return nil, fmt.Errorf("%w: invalid blob cid", ErrProtocol)
		}
		out = append(out, *parsed)
	}
	return out, nil
}

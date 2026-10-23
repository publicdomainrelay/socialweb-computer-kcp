package xrpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/websocket"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotoauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

const (
	nsidSubscribeClusters = NSIDPrefix + "subscribeClusters"

	DefaultStreamLifetime = time.Hour

	DefaultStreamsPerCaller = 8

	streamQueue     = 512
	streamReadLimit = 1 << 12
	pingEvery       = 30 * time.Second
	pongWait        = 75 * time.Second
	frameWriteLimit = 10 * time.Second
)

var drisl = func() cbor.EncMode {
	m, err := cbor.EncOptions{Sort: cbor.SortLengthFirst}.EncMode()
	if err != nil {
		panic("xrpcserver: the DRISL-CBOR encoder options were refused: " + err.Error())
	}
	return m
}()

type frameHeader struct {
	Op int64  `cbor:"op"`
	T  string `cbor:"t,omitempty"`
}

type errorFrame struct {
	Error   string `cbor:"error"`
	Message string `cbor:"message,omitempty"`
}

func encodeFrame(h frameHeader, payload any) ([]byte, error) {
	head, err := drisl.Marshal(h)
	if err != nil {
		return nil, err
	}
	body, err := drisl.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

func messageFrame(ev gateway.Event) ([]byte, error) {
	return encodeFrame(frameHeader{Op: 1, T: "#" + ev.Type}, ev.Payload)
}

func errFrame(xe *gateway.XRPCError) []byte {
	b, err := encodeFrame(frameHeader{Op: -1}, errorFrame{Error: xe.Name, Message: xe.Message})
	if err != nil {
		panic("xrpcserver: an error frame did not encode: " + err.Error())
	}
	return b
}

type streams struct {
	wg       sync.WaitGroup
	mu       sync.Mutex
	open     map[gateway.Caller]int
	limit    int
	ctx      context.Context
	shutdown context.CancelFunc
}

func newStreams(ctx context.Context, limit int) *streams {
	ctx, cancel := context.WithCancel(ctx)
	return &streams{open: map[gateway.Caller]int{}, limit: limit, ctx: ctx, shutdown: cancel}
}

func (s *streams) acquire(c gateway.Caller) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[c] >= s.limit {
		return false
	}
	s.open[c]++
	return true
}

func (s *streams) release(c gateway.Caller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[c]--; s.open[c] <= 0 {
		delete(s.open, c)
	}
}

const closeWait = 3 * time.Second

func (s *Server) Close() {
	s.streams.shutdown()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.streams.ctx), closeWait)
	defer cancel()
	_ = supervise.Join(ctx, s.streams.wg.Wait)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 10,
	WriteBufferSize: 1 << 12,
	CheckOrigin:     func(*http.Request) bool { return true },
}

func (s *Server) subscribeClusters(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		httpjson.Write(w, http.StatusMethodNotAllowed, errorBody{Error: "MethodNotAllowed", Message: "a subscription is opened with GET and a WebSocket upgrade"})
		return
	}
	if !websocket.IsWebSocketUpgrade(req) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		httpjson.Write(w, http.StatusUpgradeRequired, errorBody{Error: "UpgradeRequired", Message: "this is an event stream: connect with a WebSocket upgrade"})
		return
	}

	caller, refused := s.verifySubscriber(req)
	name := req.URL.Query().Get("name")
	if refused == nil {
		refused = s.checkSubscribeParams(req)
	}

	if refused == nil {
		if !s.streams.acquire(caller) {
			w.Header().Set("Retry-After", strconv.Itoa(int(s.opts.RetryAfter.Seconds())))
			httpjson.Write(w, http.StatusTooManyRequests, errorBody{Error: "RateLimitExceeded",
				Message: fmt.Sprintf("this account already has %d open streams", s.streams.limit)})

			return
		}
		defer s.streams.release(caller)
	}

	s.streams.wg.Add(1)
	defer s.streams.wg.Done()
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		slog.Warn("xrpc: subscription upgrade failed", "nsid", nsidSubscribeClusters, "err", err)
		return
	}
	defer conn.Close()

	if refused != nil {
		closeWithError(conn, refused)
		return
	}
	s.stream(conn, caller, name)
}

func (s *Server) verifySubscriber(req *http.Request) (gateway.Caller, *gateway.XRPCError) {
	token, ok := bearer(req.Header.Get("Authorization"))
	if !ok {
		return "", gateway.ErrUnauthorized
	}
	did, err := atprotoauth.Verify(req.Context(), token, nsidSubscribeClusters, s.opts.Audiences, s.opts.Directory)
	switch {
	case err == nil:
		return gateway.Caller(did), nil
	case errors.Is(err, atprotoauth.ErrExpired):
		return "", gateway.ErrTokenExpired
	case errors.Is(err, atprotoauth.ErrNoKey):
		return "", gateway.ErrClusterUnavailable
	default:
		return "", gateway.ErrUnauthorized
	}
}

func (s *Server) checkSubscribeParams(req *http.Request) *gateway.XRPCError {
	def := s.params[nsidSubscribeClusters]
	params, err := queryParams(req.URL.Query(), def)
	if err != nil {
		return gateway.ErrInvalidRequest
	}
	if err := validate(nsidSubscribeClusters+" parameters", s.catalog, def, params); err != nil {
		return gateway.ErrInvalidRequest
	}
	if name, ok := params["name"].(string); ok && labelOrInvalidRequest(name) != nil {
		return gateway.ErrInvalidRequest
	}
	return nil
}

func closeWithError(conn *websocket.Conn, xe *gateway.XRPCError) {
	_ = conn.SetWriteDeadline(time.Now().Add(frameWriteLimit))
	if err := conn.WriteMessage(websocket.BinaryMessage, errFrame(xe)); err != nil {
		return
	}
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, xe.Name), time.Now().Add(frameWriteLimit))
}

func (s *Server) stream(conn *websocket.Conn, caller gateway.Caller, name string) {
	lifetime := s.opts.StreamLifetime
	ctx, cancel := context.WithTimeout(s.streams.ctx, lifetime)
	defer cancel()

	frames := make(chan []byte, streamQueue)
	emit := func(ev gateway.Event) error {
		b, err := messageFrame(ev)
		if err != nil {
			return err
		}
		select {
		case frames <- b:
			return nil
		default:
			return gateway.ErrConsumerTooSlow
		}
	}

	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		defer cancel()
		conn.SetReadLimit(streamReadLimit)
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(pongWait)) })
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()
	defer func() {
		_ = conn.Close()
		<-clientGone
	}()

	done := make(chan error, 1)
	go func() { done <- s.gateway.SubscribeClusters(ctx, caller, name, emit) }()

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	write := func(b []byte) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(frameWriteLimit))
		return conn.WriteMessage(websocket.BinaryMessage, b) == nil
	}
	slog.Info("xrpc: subscription open", "nsid", nsidSubscribeClusters, "caller", string(caller), "name", name)
	for {
		select {
		case b := <-frames:
			if !write(b) {
				cancel()
				<-done
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(frameWriteLimit)); err != nil {
				cancel()
				<-done
				return
			}
		case err := <-done:
			for drained := errors.Is(err, gateway.ErrConsumerTooSlow); !drained; {
				select {
				case b := <-frames:
					if !write(b) {
						return
					}
				default:
					drained = true
				}
			}
			s.finish(conn, caller, name, ctx, clientGone, err)
			return
		}
	}
}

func (s *Server) finish(conn *websocket.Conn, caller gateway.Caller, name string, ctx context.Context, clientGone <-chan struct{}, err error) {
	select {
	case <-clientGone:
		slog.Info("xrpc: subscription closed by the client", "caller", string(caller), "name", name)
		return
	default:
	}
	if s.streams.ctx.Err() != nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "the gateway is shutting down; reconnect"),
			time.Now().Add(frameWriteLimit))
		return
	}
	var xe *gateway.XRPCError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		xe = &gateway.XRPCError{Name: gateway.ErrTokenExpired.Name, Status: 401,
			Message: fmt.Sprintf("the stream reached its %s lifetime bound; get a fresh token and reconnect", s.opts.StreamLifetime)}
	case errors.As(err, &xe):
	case err == nil:
		xe = &gateway.XRPCError{Name: "ClusterUnavailable", Status: 503, Message: "the cluster feed ended"}
	default:
		slog.Error("xrpc: subscription failed", "caller", string(caller), "name", name, "err", err)
		xe = &gateway.XRPCError{Name: "ClusterUnavailable", Status: 503, Message: "the cluster feed stopped"}
	}
	slog.Info("xrpc: subscription ended", "caller", string(caller), "name", name, "error", xe.Name, "message", xe.Message)
	closeWithError(conn, xe)
}

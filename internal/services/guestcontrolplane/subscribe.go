package guestcontrolplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

const (
	SubscribeEventsPath = "/xrpc/" + market.ComputeGuestSubscribeEventsNSID

	SubscribeKeepalive = 25 * time.Second

	subscribeWriteTimeout = 10 * time.Second
)

var subscribeUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

type eventFrame struct {
	Type string `json:"$type"`

	guestcp.QueuedEvent
}

type errorFrame struct {
	Error string `json:"error"`

	Message string `json:"message,omitempty"`
}

func (s *Service) openStream(ctx context.Context, authorization string, cursor *int64) (string, guestcp.EventStream, error) {
	guest := s.guest()
	s.mu.Lock()
	queue := s.queue
	s.mu.Unlock()
	if guest == nil || queue == nil {
		return "", nil, guestcp.ErrQueueNotOpen
	}
	name, err := guest.Authenticate(ctx, authorization)
	if err != nil {
		return "", nil, err
	}
	stream, err := queue.Subscribe(name, cursor)
	return name, stream, err
}

func relayedAuthorization(headers map[string]string) string {
	for name, value := range headers {
		if strings.EqualFold(name, "Authorization") {
			return value
		}
	}
	return ""
}

func parseCursor(raw string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, fmt.Errorf("cursor %q is not a non-negative integer", raw)
	}
	return &value, nil
}

func subscribeErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, guestcp.ErrUnauthenticated):
		return http.StatusUnauthorized, "AuthenticationRequired"
	case errors.Is(err, guestcp.ErrUnknownGuest):
		return http.StatusNotFound, "UnknownGuest"
	case errors.Is(err, guestcp.ErrFutureCursor):
		return http.StatusBadRequest, market.GuestSubscribeEventsErrFutureCursor
	default:
		return http.StatusServiceUnavailable, "Unavailable"
	}
}

func encodeEvent(event guestcp.QueuedEvent) ([]byte, error) {
	return json.Marshal(eventFrame{Type: market.ComputeGuestSubscribeEventsNSID + "#event", QueuedEvent: event})
}

func (s *Service) handleSubscribeEvents(w http.ResponseWriter, r *http.Request) {
	cursor, err := parseCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeGuestError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, stream, err := s.openStream(r.Context(), r.Header.Get("Authorization"), cursor)
	if err != nil && !errors.Is(err, guestcp.ErrFutureCursor) {
		status, code := subscribeErrorStatus(err)
		httpjson.Write(w, status, errorFrame{Error: code, Message: err.Error()})
		return
	}
	conn, upgradeErr := subscribeUpgrader.Upgrade(w, r, nil)
	if upgradeErr != nil {
		if stream != nil {
			stream.Close()
		}
		return
	}
	defer conn.Close()
	if err != nil {
		raw, _ := json.Marshal(errorFrame{Error: market.GuestSubscribeEventsErrFutureCursor, Message: err.Error()})
		_ = conn.SetWriteDeadline(time.Now().Add(subscribeWriteTimeout))
		_ = conn.WriteMessage(websocket.TextMessage, raw)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, market.GuestSubscribeEventsErrFutureCursor), time.Now().Add(time.Second))
		return
	}
	defer stream.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	write := func(kind int, data []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(subscribeWriteTimeout))
		return conn.WriteMessage(kind, data)
	}
	s.log.Info("a guest subscribes to its events", "computevm", name, "cursor", cursorValue(cursor))
	err = s.streamEvents(ctx, stream, func(frame []byte) error { return write(websocket.TextMessage, frame) },
		func() error { return write(websocket.BinaryMessage, nil) })
	if errors.Is(err, guestcp.ErrQueueGone) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "the ComputeVM is gone"), time.Now().Add(time.Second))
	}
	s.log.Info("a guest event subscription ends", "computevm", name, "reason", errString(err))
}

func (s *Service) subscribeDirect(ctx context.Context) func(string, string, map[string]string, map[string]string, func(json.RawMessage), func([]byte)) (func(), error) {
	return func(_, nsid string, params, headers map[string]string, onEvent func(json.RawMessage), onData func([]byte)) (func(), error) {
		if nsid != market.ComputeGuestSubscribeEventsNSID {
			return nil, fmt.Errorf("guestcontrolplane: no subscription %s", nsid)
		}
		cursor, err := parseCursor(params["cursor"])
		if err != nil {
			return nil, err
		}
		name, stream, err := s.openStream(ctx, relayedAuthorization(headers), cursor)
		if err != nil {
			_, code := subscribeErrorStatus(err)
			return nil, fmt.Errorf("%s: %v", code, err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.log.Info("a guest subscribes to its events through the relay", "computevm", name, "cursor", cursorValue(cursor))
		s.wg.Go(func() {
			defer stream.Close()
			err := s.streamEvents(runCtx, stream, func(frame []byte) error { onEvent(frame); return nil },
				func() error { onData(nil); return nil })
			s.log.Info("a guest event subscription through the relay ends", "computevm", name, "reason", errString(err))
		})
		return cancel, nil
	}
}

func (s *Service) streamEvents(ctx context.Context, stream guestcp.EventStream, send func([]byte) error, keepalive func() error) error {
	events := make(chan guestcp.QueuedEvent)
	failed := make(chan error, 1)
	go func() {
		for {
			event, err := stream.Next(ctx)
			if err != nil {
				failed <- err
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(SubscribeKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failed:
			return err
		case event := <-events:
			frame, err := encodeEvent(event)
			if err != nil {
				return err
			}
			if err := send(frame); err != nil {
				return err
			}
		case <-ticker.C:
			if err := keepalive(); err != nil {
				return err
			}
		}
	}
}

func cursorValue(cursor *int64) string {
	if cursor == nil {
		return ""
	}
	return strconv.FormatInt(*cursor, 10)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

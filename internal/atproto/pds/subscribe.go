package pds

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/websocket"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
)

const (
	subscribeBackfillPage = 500
	subscribeWriteTimeout = 15 * time.Second
	subscribePingInterval = 30 * time.Second
	subscribeReadTimeout  = 120 * time.Second
)

type subscribeFrameHeader struct {
	Op int64  `cbor:"op"`
	T  string `cbor:"t"`
}

var subscribeUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   4096,
	WriteBufferSize:  16384,
	CheckOrigin: func(*http.Request) bool {
		return true
	},
}

func (s *Server) handleSubscribeRepos(w http.ResponseWriter, r *http.Request) {
	cursorRaw := r.URL.Query().Get("cursor")
	hasCursor := cursorRaw != ""
	var cursor int64
	if hasCursor {
		n, err := strconv.ParseInt(cursorRaw, 10, 64)
		if err != nil || n < 0 {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "cursor must be a non-negative integer")
			return
		}
		cursor = n
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "websocket upgrade required")
		return
	}
	conn, err := subscribeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	curCh, cancelSub := s.seq.Subscribe()
	defer cancelSub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.SetReadLimit(1 << 20)
		_ = conn.SetReadDeadline(time.Now().Add(subscribeReadTimeout))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(subscribeReadTimeout))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	last := int64(-1)
	if hasCursor {
		last = cursor
		for {
			events, err := s.repos.ListEvents(ctx, last, subscribeBackfillPage)
			if err != nil {
				return
			}
			for _, ev := range events {
				if ev.Seq <= last {
					continue
				}
				if err := s.writeCommitFrame(conn, ev); err != nil {
					return
				}
				last = ev.Seq
			}
			if len(events) < subscribeBackfillPage {
				break
			}
		}
	}

	ticker := time.NewTicker(subscribePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case ev, ok := <-curCh:
			if !ok {
				return
			}
			if ev.Seq <= last {
				continue
			}
			if err := s.writeCommitFrame(conn, ev); err != nil {
				return
			}
			last = ev.Seq
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(subscribeWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Server) writeCommitFrame(conn *websocket.Conn, ev *repo.CommitEvent) error {
	header, err := cbor.Marshal(subscribeFrameHeader{Op: 1, T: "#commit"})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	if err := ev.CommitMessage().MarshalCBOR(&body); err != nil {
		return err
	}
	frame := make([]byte, 0, len(header)+body.Len())
	frame = append(frame, header...)
	frame = append(frame, body.Bytes()...)
	_ = conn.SetWriteDeadline(time.Now().Add(subscribeWriteTimeout))
	return conn.WriteMessage(websocket.BinaryMessage, frame)
}

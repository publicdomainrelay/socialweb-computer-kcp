package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	subscribeBackoffMin = 2 * time.Second

	subscribeBackoffMax = 30 * time.Second

	subscribeReadTimeout = 90 * time.Second

	subscribeDialTimeout = 20 * time.Second

	maxFrame = 1 << 20
)

var errFutureCursor = errors.New("guestagent: the bidder has no events up to the cursor")

type eventFrame struct {
	Type string `json:"$type"`

	Seq int64 `json:"seq"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Record json.RawMessage `json:"record"`

	Error string `json:"error"`

	Message string `json:"message"`
}

type guestRecord struct {
	Type string `json:"$type"`

	Kind string `json:"kind"`

	Tag string `json:"tag"`

	Ciphertext string `json:"ciphertext"`
}

func (a *Agent) subscribe(ctx context.Context) {
	backoff := subscribeBackoffMin
	for ctx.Err() == nil {
		connected, err := a.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = subscribeBackoffMin
		}
		if errors.Is(err, errFutureCursor) {
			a.log.Warn("the bidder answered FutureCursor; restarting the subscription from cursor 0", "cursor", a.cursor)
			a.saveCursor(0)
		}
		a.log.Warn("the guest event subscription closed; reconnecting", "err", errText(err), "cursor", a.cursor, "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, subscribeBackoffMax)
	}
}

func (a *Agent) subscribeURL() (string, error) {
	u, err := url.Parse(a.cfg.Bidder.Base + SubscribeEventsPath)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.RawQuery = url.Values{"cursor": {strconv.FormatInt(a.cursor, 10)}}.Encode()
	return u.String(), nil
}

func (a *Agent) stream(ctx context.Context) (bool, error) {
	target, err := a.subscribeURL()
	if err != nil {
		return false, err
	}
	token, err := a.tokens.Token(ctx)
	if err != nil {
		return false, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: subscribeDialTimeout, Proxy: nil}
	conn, resp, err := dialer.DialContext(ctx, target, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				a.tokens.Invalidate(token)
			}
			return false, fmt.Errorf("upgrade refused: http %d", resp.StatusCode)
		}
		return false, err
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrame)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	a.log.Info("the guest event subscription is open", "cursor", a.cursor)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(subscribeReadTimeout))
		kind, data, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.ClosePolicyViolation && strings.Contains(closeErr.Text, errFutureCursorName) {
				return true, errFutureCursor
			}
			return true, err
		}
		if kind != websocket.TextMessage {
			continue
		}
		var frame eventFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			a.log.Warn("a subscription frame is not JSON", "bytes", len(data))
			continue
		}
		if frame.Error != "" {
			if frame.Error == errFutureCursorName {
				return true, errFutureCursor
			}
			return true, fmt.Errorf("the bidder ended the subscription: %s: %s", frame.Error, tail(frame.Message, 256))
		}
		a.receive(frame)
		if frame.Seq > a.cursor {
			a.saveCursor(frame.Seq)
		}
	}
}

func (a *Agent) receive(frame eventFrame) {
	var record guestRecord
	if err := json.Unmarshal(frame.Record, &record); err != nil {
		a.log.Warn("a guest event record is not a JSON object", "seq", frame.Seq)
		return
	}
	id := frame.URI + "#" + frame.CID
	if a.seen[id] {
		a.log.Debug("a guest event was seen before", "seq", frame.Seq)
		return
	}
	a.seen[id] = true
	_, known := a.cfg.Port(record.Tag)
	switch {
	case record.Type == SecretsNSID:
		a.sealedSecrets(a.ctx, frame.Seq, record.Ciphertext)
	case record.Type == PublicKeyNSID:
		a.log.Debug("requester publicKey for tag "+record.Tag, "seq", frame.Seq, "tag", record.Tag, "known", known)
	case record.Type == AgentStartNSID:
		a.startAgent(frame.Seq)
	case record.Type != SignalWebRTCNSID:
		a.log.Debug("a guest event of another collection", "seq", frame.Seq, "collection", record.Type)
	case !known:
		a.log.Debug("a signal for a tag this guest does not serve", "seq", frame.Seq, "tag", record.Tag)
	case record.Kind != signalKindOffer:
		a.log.Debug("a signal that is not an offer", "seq", frame.Seq, "tag", record.Tag, "kind", record.Kind)
	default:
		a.offer(frame.Seq, record.Tag, record.Ciphertext)
	}
}

func (a *Agent) loadCursor() {
	raw, err := os.ReadFile(filepath.Join(a.cfg.StateDir, CursorFile))
	if err != nil {
		return
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || value < 0 {
		a.log.Warn("the saved cursor is not a non-negative integer; starting from 0", "bytes", len(raw))
		return
	}
	a.cursor = value
}

func (a *Agent) saveCursor(value int64) {
	a.cursor = value
	path := filepath.Join(a.cfg.StateDir, CursorFile)
	part := path + ".part"
	if err := os.WriteFile(part, []byte(strconv.FormatInt(value, 10)+"\n"), 0o600); err != nil {
		a.log.Warn("saving the cursor failed", "err", err.Error())
		return
	}
	if err := os.Rename(part, path); err != nil {
		a.log.Warn("saving the cursor failed", "err", err.Error())
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

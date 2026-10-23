package wsconn

import (
	"cmp"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	HandshakeTimeout = 15 * time.Second

	ReadTimeout = 2 * time.Minute

	MaxFrameBytes = 8 << 20
)

type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}

func Parse(raw string) (*url.URL, error) {
	parsed, err := url.Parse(Normalize(raw))
	if err != nil {
		return nil, fmt.Errorf("wsconn: %q: %w", raw, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return nil, fmt.Errorf("wsconn: %q is not an http, https, ws or wss URL", raw)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil, fmt.Errorf("wsconn: %q has no host", raw)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("wsconn: %q must not carry user info", raw)
	}
	return parsed, nil
}

func WebSocketScheme(u *url.URL) {
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
}

type Conn struct {
	conn *websocket.Conn
}

func Dial(ctx context.Context, target string, dial DialFunc) (*Conn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: HandshakeTimeout,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if dial != nil {
		dialer.NetDialContext = dial
	}
	conn, response, err := dialer.DialContext(ctx, target, nil)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("wsconn: websocket handshake: %s", cmp.Or(http.StatusText(response.StatusCode), "websocket handshake failed"))
		}
		return nil, fmt.Errorf("wsconn: websocket dial: %w", err)
	}
	conn.SetReadLimit(MaxFrameBytes)
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(ReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	_ = conn.SetReadDeadline(time.Now().Add(ReadTimeout))
	return &Conn{conn: conn}, nil
}

func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(ReadTimeout))
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetReadDeadline(time.Now()) })
	defer stop()
	_, data, err := c.conn.ReadMessage()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return data, nil
}

func (c *Conn) Close() error {
	return c.conn.Close()
}

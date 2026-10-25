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
)

const (
	HandshakeTimeout = 15 * time.Second

	ReadTimeout = 2 * time.Minute

	MaxFrameBytes = 8 << 20

	PongWriteTimeout = 10 * time.Second

	MaxBufferedBytes = 2 * MaxFrameBytes
)

type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

type Config struct {
	Dial             DialFunc
	TLSConfig        *tls.Config
	HandshakeTimeout time.Duration
}

type HandshakeError struct {
	StatusCode int
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("wsconn: websocket handshake: %s", cmp.Or(http.StatusText(e.StatusCode), "websocket handshake failed"))
}

type CloseError struct {
	Code   int
	Reason string
	Clean  bool
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("wsconn: websocket closed: code %d reason %q clean %t", e.Code, e.Reason, e.Clean)
}

func Dial(ctx context.Context, target string, dial DialFunc) (*Conn, error) {
	return DialConfig(ctx, target, Config{Dial: dial})
}

func (c Config) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return HandshakeTimeout
}

func (c Config) tlsConfig() *tls.Config {
	if c.TLSConfig != nil {
		return c.TLSConfig
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

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

//go:build !js

package wsconn

import (
	"context"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

type Conn struct {
	conn *websocket.Conn
}

func DialConfig(ctx context.Context, target string, cfg Config) (*Conn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: cfg.handshakeTimeout(),
		TLSClientConfig:  cfg.tlsConfig(),
	}
	if cfg.Dial != nil {
		dialer.NetDialContext = cfg.Dial
	}
	conn, response, err := dialer.DialContext(ctx, target, nil)
	if err != nil {
		if response != nil {
			return nil, &HandshakeError{StatusCode: response.StatusCode}
		}
		return nil, fmt.Errorf("wsconn: websocket dial: %w", err)
	}
	conn.SetReadLimit(MaxFrameBytes)
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(ReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(PongWriteTimeout))
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

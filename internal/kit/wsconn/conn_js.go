//go:build js

package wsconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall/js"
	"time"
)

const (
	socketConnecting = 0
	socketOpen       = 1
	closeNormal      = 1000
	closeAbnormal    = 1006
	frameOverhead    = 64
)

var socketHandlers = []string{"onopen", "onmessage", "onerror", "onclose"}

type Conn struct {
	ws     js.Value
	ready  chan struct{}
	opened chan struct{}
	done   chan struct{}

	mu       sync.Mutex
	err      error
	isOpen   bool
	queue    [][]byte
	buffered int
	funcs    []js.Func
	released bool
}

func DialConfig(ctx context.Context, target string, cfg Config) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctor := js.Global().Get("WebSocket")
	if ctor.Type() != js.TypeFunction {
		return nil, errors.New("wsconn: websocket dial: this js runtime has no WebSocket")
	}
	ws, err := newSocket(ctor, target)
	if err != nil {
		return nil, fmt.Errorf("wsconn: websocket dial: %w", err)
	}
	c := &Conn{
		ws:     ws,
		ready:  make(chan struct{}, 1),
		opened: make(chan struct{}),
		done:   make(chan struct{}),
	}
	ws.Set("binaryType", "arraybuffer")
	c.handle("onopen", c.onOpen)
	c.handle("onmessage", c.onMessage)
	c.handle("onerror", c.onError)
	c.handle("onclose", c.onClose)
	timeout := cfg.handshakeTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.opened:
		return c, nil
	case <-c.done:
		failure := c.failure()
		_ = c.Close()
		return nil, fmt.Errorf("wsconn: websocket dial: %w", failure)
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	case <-timer.C:
		_ = c.Close()
		return nil, fmt.Errorf("wsconn: websocket dial: handshake timeout after %s: %w", timeout, os.ErrDeadlineExceeded)
	}
}

func newSocket(ctor js.Value, target string) (ws js.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return ctor.New(target), nil
}

func (c *Conn) handle(name string, fn func(js.Value)) {
	f := js.FuncOf(func(_ js.Value, args []js.Value) any {
		event := js.Undefined()
		if len(args) > 0 {
			event = args[0]
		}
		fn(event)
		return nil
	})
	c.funcs = append(c.funcs, f)
	c.ws.Set(name, f)
}

func (c *Conn) onOpen(js.Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isOpen {
		return
	}
	c.isOpen = true
	close(c.opened)
}

func (c *Conn) onError(js.Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked(&CloseError{Code: closeAbnormal, Reason: "error event"})
}

func (c *Conn) onClose(event js.Value) {
	closeErr := &CloseError{Code: closeAbnormal}
	if event.Type() == js.TypeObject {
		if code := event.Get("code"); code.Type() == js.TypeNumber {
			closeErr.Code = code.Int()
		}
		if reason := event.Get("reason"); reason.Type() == js.TypeString {
			closeErr.Reason = reason.String()
		}
		closeErr.Clean = event.Get("wasClean").Truthy()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked(closeErr)
}

func (c *Conn) onMessage(event js.Value) {
	frame, err := frameOf(event)
	if err == nil {
		err = c.buffer(frame)
	}
	if err != nil {
		c.fail(err)
	}
}

func (c *Conn) buffer(frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil
	}
	size := len(frame) + frameOverhead
	if c.buffered+size > MaxBufferedBytes {
		return fmt.Errorf("wsconn: websocket read buffer is over %d bytes in %d frames", MaxBufferedBytes, len(c.queue))
	}
	c.queue = append(c.queue, frame)
	c.buffered += size
	select {
	case c.ready <- struct{}{}:
	default:
	}
	return nil
}

func frameOf(event js.Value) ([]byte, error) {
	if event.Type() != js.TypeObject {
		return nil, errors.New("wsconn: websocket message without an event")
	}
	data := event.Get("data")
	switch data.Type() {
	case js.TypeString:
		frame := []byte(data.String())
		if len(frame) > MaxFrameBytes {
			return nil, frameTooBig(len(frame))
		}
		return frame, nil
	case js.TypeObject:
		size := data.Get("byteLength")
		if size.Type() != js.TypeNumber {
			return nil, errors.New("wsconn: websocket message is neither text nor an ArrayBuffer")
		}
		n := size.Int()
		if n > MaxFrameBytes {
			return nil, frameTooBig(n)
		}
		frame := make([]byte, n)
		if n > 0 {
			js.CopyBytesToGo(frame, js.Global().Get("Uint8Array").New(data))
		}
		return frame, nil
	default:
		return nil, errors.New("wsconn: websocket message is neither text nor an ArrayBuffer")
	}
}

func frameTooBig(n int) error {
	return fmt.Errorf("wsconn: websocket frame of %d bytes is over the %d byte limit", n, MaxFrameBytes)
}

func (c *Conn) endLocked(err error) bool {
	if c.err != nil {
		return false
	}
	c.err = err
	close(c.done)
	return true
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	ended := c.endLocked(err)
	c.mu.Unlock()
	if ended {
		c.closeSocket()
	}
}

func (c *Conn) closeSocket() {
	defer func() { _ = recover() }()
	switch c.ws.Get("readyState").Int() {
	case socketConnecting, socketOpen:
		c.ws.Call("close", closeNormal)
	}
}

func (c *Conn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) next() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return nil, false
	}
	frame := c.queue[0]
	c.queue[0] = nil
	c.queue = c.queue[1:]
	if len(c.queue) == 0 {
		c.queue = nil
	}
	c.buffered -= len(frame) + frameOverhead
	return frame, true
}

func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frame, ok := c.next(); ok {
		return frame, nil
	}
	timer := time.NewTimer(ReadTimeout)
	defer timer.Stop()
	for {
		select {
		case <-c.ready:
			if frame, ok := c.next(); ok {
				return frame, nil
			}
		case <-c.done:
			if frame, ok := c.next(); ok {
				return frame, nil
			}
			return nil, c.failure()
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			c.fail(fmt.Errorf("wsconn: websocket read: no frame in %s: %w", ReadTimeout, os.ErrDeadlineExceeded))
			return nil, c.failure()
		}
	}
}

func (c *Conn) Close() error {
	c.mu.Lock()
	c.endLocked(fmt.Errorf("wsconn: %w", net.ErrClosed))
	if c.released {
		c.mu.Unlock()
		return nil
	}
	c.released = true
	funcs := c.funcs
	c.funcs = nil
	c.mu.Unlock()
	for _, name := range socketHandlers {
		c.ws.Set(name, js.Null())
	}
	c.closeSocket()
	for _, f := range funcs {
		f.Release()
	}
	return nil
}

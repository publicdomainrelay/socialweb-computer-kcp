package webrtcproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	chunk = 16 << 10

	highWater = 1 << 20

	lowWater = 256 << 10

	drainTimeout = 30 * time.Second
)

var ErrClosed = errors.New("webrtcproxy: the datachannel is closed")

type Conn struct {
	pc *webrtc.PeerConnection

	dc *webrtc.DataChannel

	log *slog.Logger

	in *io.PipeReader

	inW *io.PipeWriter

	opened chan struct{}

	low chan struct{}

	done chan struct{}

	failed chan struct{}

	failOnce sync.Once

	closeOnce sync.Once

	mu sync.Mutex

	sent int64

	received int64
}

func newConn(tag string, log *slog.Logger) (*Conn, error) {
	pc, err := newAPI(log).NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{STUNServer}}}})
	if err != nil {
		return nil, fmt.Errorf("webrtcproxy: the peer connection: %w", err)
	}
	ordered := true
	dc, err := pc.CreateDataChannel(tag, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("webrtcproxy: the datachannel: %w", err)
	}
	in, inW := io.Pipe()
	c := &Conn{
		pc:     pc,
		dc:     dc,
		log:    log,
		in:     in,
		inW:    inW,
		opened: make(chan struct{}),
		low:    make(chan struct{}, 1),
		done:   make(chan struct{}),
		failed: make(chan struct{}),
	}
	dc.SetBufferedAmountLowThreshold(lowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case c.low <- struct{}{}:
		default:
		}
	})
	dc.OnOpen(func() { close(c.opened) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if _, err := c.inW.Write(msg.Data); err != nil {
			return
		}
		c.mu.Lock()
		c.received += int64(len(msg.Data))
		c.mu.Unlock()
	})
	dc.OnClose(func() { c.finish(io.EOF) })
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Info("the ICE connection state changed", "state", state.String())
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Info("the peer connection state changed", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed:
			c.finish(fmt.Errorf("webrtcproxy: the peer connection failed (ICE %s)", pc.ICEConnectionState()))
		case webrtc.PeerConnectionStateClosed:
			c.finish(io.EOF)
		}
	})
	return c, nil
}

func (c *Conn) finish(err error) {
	c.failOnce.Do(func() {
		close(c.failed)
		_ = c.inW.CloseWithError(err)
	})
}

func (c *Conn) waitOpen(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case <-c.opened:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.failed:
		return fmt.Errorf("webrtcproxy: the peer connection failed before the datachannel opened: ICE %s, connection %s; a symmetric NAT on either side defeats STUN-only WebRTC (stun:stun.cloudflare.com:3478, no TURN)",
			c.pc.ICEConnectionState(), c.pc.ConnectionState())
	case <-deadline.C:
		return fmt.Errorf("webrtcproxy: the datachannel did not open within %s: ICE %s, connection %s; a symmetric NAT on either side defeats STUN-only WebRTC (stun:stun.cloudflare.com:3478, no TURN)",
			timeout, c.pc.ICEConnectionState(), c.pc.ConnectionState())
	}
}

func (c *Conn) Read(p []byte) (int, error) {
	return c.in.Read(p)
}

func (c *Conn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n := min(len(p)-written, chunk)
		if err := c.dc.Send(p[written : written+n]); err != nil {
			return written, fmt.Errorf("webrtcproxy: sending on the datachannel: %w", err)
		}
		written += n
		c.mu.Lock()
		c.sent += int64(n)
		c.mu.Unlock()
		if err := c.waitBelow(highWater); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *Conn) waitBelow(limit uint64) error {
	deadline := time.NewTimer(drainTimeout)
	defer deadline.Stop()
	for c.dc.BufferedAmount() >= limit {
		select {
		case <-c.low:
		case <-c.done:
			return ErrClosed
		case <-c.failed:
			return ErrClosed
		case <-deadline.C:
			deadline.Reset(drainTimeout)
			if c.dc.ReadyState() != webrtc.DataChannelStateOpen {
				return ErrClosed
			}
		}
	}
	return nil
}

func (c *Conn) Drain() error {
	c.dc.SetBufferedAmountLowThreshold(0)
	return c.waitBelow(1)
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.finish(io.EOF)
		_ = c.dc.Close()
		_ = c.pc.Close()
		c.mu.Lock()
		sent, received := c.sent, c.received
		c.mu.Unlock()
		c.log.Info("the datachannel session ends", "sentBytes", sent, "receivedBytes", received)
	})
	return nil
}

func Pipe(ctx context.Context, c *Conn, stdin io.Reader, stdout io.Writer) error {
	remote := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdout, c)
		remote <- err
	}()
	local := make(chan error, 1)
	go func() {
		_, err := io.Copy(c, stdin)
		if err == nil {
			err = c.Drain()
		}
		local <- err
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-remote:
			return err
		case err := <-local:
			if err != nil {
				return err
			}
			local = nil
		}
	}
}

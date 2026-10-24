package guestagent

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	bridgeChunk = 16 << 10

	bridgeHighWater = 1 << 20

	bridgeLowWater = 256 << 10

	bridgeDrainTimeout = 30 * time.Second

	bridgeDialTimeout = 10 * time.Second
)

type bridge struct {
	dc *webrtc.DataChannel

	port int

	log *slog.Logger

	ready chan struct{}

	low chan struct{}

	done chan struct{}

	mu sync.Mutex

	conn net.Conn

	once sync.Once

	sent int64

	received int64
}

func (s *session) onDataChannel(dc *webrtc.DataChannel) {
	log := s.log.With("channel", channelID(dc))
	if dc.Label() != s.tag {
		log.Warn("a datachannel label is not the session tag; closing it", "labelBytes", len(dc.Label()))
		go func() { _ = dc.Close() }()
		return
	}
	b := &bridge{
		dc:    dc,
		port:  s.port,
		log:   log,
		ready: make(chan struct{}),
		low:   make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	dc.SetBufferedAmountLowThreshold(bridgeLowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case b.low <- struct{}{}:
		default:
		}
	})
	dc.OnMessage(b.fromChannel)
	dc.OnClose(func() { b.finish("the datachannel closed") })
	dc.OnOpen(func() { go b.toChannel() })
	go b.dial()
}

func channelID(dc *webrtc.DataChannel) string {
	if id := dc.ID(); id != nil {
		return strconv.Itoa(int(*id))
	}
	return "unset"
}

func (b *bridge) dial() {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(b.port)), bridgeDialTimeout)
	if err != nil {
		b.log.Warn("dialing the tunnel port failed", "port", b.port, "err", err.Error())
		close(b.ready)
		b.finish("the tunnel port is not reachable")
		return
	}
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()
	close(b.ready)
	b.log.Info("a datachannel is bridged to the tunnel port", "port", b.port)
	select {
	case <-b.done:
		_ = conn.Close()
	default:
	}
}

func (b *bridge) tcp() net.Conn {
	select {
	case <-b.ready:
	case <-b.done:
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn
}

func (b *bridge) fromChannel(msg webrtc.DataChannelMessage) {
	conn := b.tcp()
	if conn == nil {
		return
	}
	if _, err := conn.Write(msg.Data); err != nil {
		b.finish("writing to the tunnel port failed")
		return
	}
	b.mu.Lock()
	b.received += int64(len(msg.Data))
	b.mu.Unlock()
}

func (b *bridge) toChannel() {
	conn := b.tcp()
	if conn == nil {
		return
	}
	buf := make([]byte, bridgeChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if sendErr := b.dc.Send(buf[:n]); sendErr != nil {
				b.finish("sending on the datachannel failed")
				return
			}
			b.mu.Lock()
			b.sent += int64(n)
			b.mu.Unlock()
			if !b.waitBelow(bridgeHighWater) {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				b.finish("reading the tunnel port failed")
				return
			}
			b.dc.SetBufferedAmountLowThreshold(0)
			if !b.waitBelow(1) {
				return
			}
			b.finish("the tunnel port closed")
			return
		}
	}
}

func (b *bridge) waitBelow(limit uint64) bool {
	deadline := time.NewTimer(bridgeDrainTimeout)
	defer deadline.Stop()
	for b.dc.BufferedAmount() >= limit {
		select {
		case <-b.low:
		case <-b.done:
			return false
		case <-deadline.C:
			deadline.Reset(bridgeDrainTimeout)
			if b.dc.ReadyState() != webrtc.DataChannelStateOpen {
				b.finish("the datachannel is not open")
				return false
			}
		}
	}
	return true
}

func (b *bridge) finish(reason string) {
	b.once.Do(func() {
		close(b.done)
		b.mu.Lock()
		conn, sent, received := b.conn, b.sent, b.received
		b.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		go func() { _ = b.dc.Close() }()
		b.log.Info("a datachannel bridge ends", "reason", reason, "sentBytes", sent, "receivedBytes", received)
	})
}

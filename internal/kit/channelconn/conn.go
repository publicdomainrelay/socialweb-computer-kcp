package channelconn

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const chunk = 16 << 10

type addr struct {
	network string
	address string
}

func (a addr) Network() string { return a.network }

func (a addr) String() string { return a.address }

type Conn struct {
	send func([]byte) error

	shut func() error

	in chan []byte

	done chan struct{}

	once sync.Once

	err error

	mu sync.Mutex

	buf []byte

	readDeadline time.Time

	writeDeadline time.Time
}

func New(send func([]byte) error, shut func() error) *Conn {
	if send == nil {
		send = func([]byte) error { return io.ErrClosedPipe }
	}
	if shut == nil {
		shut = func() error { return nil }
	}
	return &Conn{
		send: send,
		shut: shut,
		in:   make(chan []byte, 64),
		done: make(chan struct{}),
	}
}

func (c *Conn) Push(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	select {
	case <-c.done:
		return false
	default:
	}
	copied := append([]byte(nil), data...)
	select {
	case c.in <- copied:
		return true
	case <-c.done:
		return false
	}
}

func (c *Conn) CloseWithError(err error) {
	if err == nil {
		err = io.EOF
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		c.mu.Unlock()
		return n, nil
	}
	deadline := c.readDeadline
	c.mu.Unlock()
	var expire <-chan time.Time
	if !deadline.IsZero() {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(left)
		defer timer.Stop()
		expire = timer.C
	}
	select {
	case data := <-c.in:
		n := copy(p, data)
		if n < len(data) {
			c.mu.Lock()
			c.buf = append(c.buf, data[n:]...)
			c.mu.Unlock()
		}
		return n, nil
	case <-c.done:
		select {
		case data := <-c.in:
			n := copy(p, data)
			if n < len(data) {
				c.mu.Lock()
				c.buf = append(c.buf, data[n:]...)
				c.mu.Unlock()
			}
			return n, nil
		default:
			return 0, c.closeError()
		}
	case <-expire:
		return 0, os.ErrDeadlineExceeded
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		select {
		case <-c.done:
			return written, c.closeError()
		default:
		}
		if err := c.writeExpired(); err != nil {
			return written, err
		}
		n := min(len(p)-written, chunk)
		if err := c.send(p[written : written+n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (c *Conn) writeExpired() error {
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}
	return nil
}

func (c *Conn) Close() error {
	c.CloseWithError(io.EOF)
	return c.shut()
}

func (c *Conn) closeError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return io.EOF
	}
	return c.err
}

func (c *Conn) LocalAddr() net.Addr { return addr{network: "webrtc", address: "local"} }

func (c *Conn) RemoteAddr() net.Addr { return addr{network: "webrtc", address: "remote"} }

func (c *Conn) SetDeadline(at time.Time) error {
	c.mu.Lock()
	c.readDeadline, c.writeDeadline = at, at
	c.mu.Unlock()
	return nil
}

func (c *Conn) SetReadDeadline(at time.Time) error {
	c.mu.Lock()
	c.readDeadline = at
	c.mu.Unlock()
	return nil
}

func (c *Conn) SetWriteDeadline(at time.Time) error {
	c.mu.Lock()
	c.writeDeadline = at
	c.mu.Unlock()
	return nil
}

var _ net.Conn = (*Conn)(nil)

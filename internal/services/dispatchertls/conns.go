package dispatchertls

import (
	"net"
	"sync"
)

type connSet struct {
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}

type trackedConn struct {
	net.Conn
	set *connSet
}

type trackingListener struct {
	net.Listener
	set *connSet
}

func newConnSet() *connSet {
	return &connSet{conns: map[*trackedConn]struct{}{}}
}

func (s *connSet) add(conn *trackedConn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *connSet) remove(conn *trackedConn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	conns := make([]*trackedConn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.conns = map[*trackedConn]struct{}{}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (l *trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, set: l.set}
	l.set.add(tracked)
	return tracked, nil
}

func (c *trackedConn) Close() error {
	c.set.remove(c)
	return c.Conn.Close()
}

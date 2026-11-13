package tcpforward

import (
	"context"
	"io"
	"net"
	"sync"
)

func Start(ctx context.Context, listen, target string) (func(), error) {
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-ctx.Done()
		_ = l.Close()
	})
	wg.Go(func() {
		var d net.Dialer
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				u, err := d.DialContext(ctx, "tcp", target)
				if err != nil {
					return
				}
				defer u.Close()
				stop := context.AfterFunc(ctx, func() { _ = c.Close(); _ = u.Close() })
				defer stop()
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(u, c)
					if t, ok := u.(*net.TCPConn); ok {
						_ = t.CloseWrite()
					}
					close(done)
				}()
				_, _ = io.Copy(c, u)
				if t, ok := c.(*net.TCPConn); ok {
					_ = t.CloseWrite()
				}
				<-done
			})
		}
	})
	return func() { cancel(); wg.Wait() }, nil
}

type Set struct {
	mu    sync.Mutex
	stops map[string]func()
	dests map[string]string
}

func (s *Set) Ensure(listen, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dests[listen] == target {
		return nil
	}
	if stop, ok := s.stops[listen]; ok {
		stop()
		delete(s.stops, listen)
		delete(s.dests, listen)
	}
	stop, err := Start(context.Background(), listen, target)
	if err != nil {
		return err
	}
	if s.stops == nil {
		s.stops, s.dests = map[string]func(){}, map[string]string{}
	}
	s.stops[listen], s.dests[listen] = stop, target
	return nil
}

func (s *Set) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for listen, stop := range s.stops {
		stop()
		delete(s.stops, listen)
		delete(s.dests, listen)
	}
}

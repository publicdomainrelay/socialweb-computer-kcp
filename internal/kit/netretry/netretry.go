package netretry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/authretry"
)

const (
	First = 2 * time.Second

	Cap = 30 * time.Second
)

func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if status := authretry.Status(err); status != 0 {
		return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func Delay(failures int) time.Duration {
	d := First
	for i := 1; i < failures && d < Cap; i++ {
		d *= 2
	}
	return min(d, Cap)
}

type Tracker[K comparable] struct {
	mu sync.Mutex

	failures map[K]int
}

func NewTracker[K comparable]() *Tracker[K] {
	return &Tracker[K]{failures: map[K]int{}}
}

func (t *Tracker[K]) Fail(key K) (int, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.failures[key] + 1
	t.failures[key] = n
	return n, Delay(n)
}

func (t *Tracker[K]) Reset(key K) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
}

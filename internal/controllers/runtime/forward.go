package runtime

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

const forwardBuffer = 64

// Go runs fn under the manager: it starts with the manager, its context ends
// when the manager stops, and the manager joins it before Start returns.
func (m *Manager) Go(name string, fn func(context.Context) error) error {
	return m.mgr.GetLocalManager().Add(manager.RunnableFunc(func(ctx context.Context) error {
		return supervise.Run(ctx, name, fn)
	}))
}

// Forward sends the requests fan returns for each value of in to the
// returned channel, until in closes or the manager stops.
func Forward[V any](m *Manager, name string, in <-chan V, fan func(V) []Request) (<-chan Request, error) {
	out := make(chan Request, forwardBuffer)
	err := m.Go(name, func(ctx context.Context) error {
		defer close(out)
		for {
			var v V
			var ok bool
			select {
			case <-ctx.Done():
				return nil
			case v, ok = <-in:
			}
			if !ok {
				return nil
			}
			for _, r := range fan(v) {
				select {
				case out <- r:
				case <-ctx.Done():
					return nil
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Queue collects requests without blocking the caller; Run drains them to
// a channel under the manager.
type Queue struct {
	mu      sync.Mutex
	pending []Request
	queued  map[Request]bool
	wake    chan struct{}
}

func NewQueue() *Queue {
	return &Queue{queued: map[Request]bool{}, wake: make(chan struct{}, 1)}
}

func (q *Queue) Add(rs ...Request) {
	q.mu.Lock()
	for _, r := range rs {
		if !q.queued[r] {
			q.queued[r] = true
			q.pending = append(q.pending, r)
		}
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) take(struct{}) []Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.pending
	q.pending = nil
	clear(q.queued)
	return out
}

func (q *Queue) Run(m *Manager, name string) (<-chan Request, error) {
	return Forward(m, name, q.wake, q.take)
}

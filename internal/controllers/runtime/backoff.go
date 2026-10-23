package runtime

import (
	"sync"
	"time"

	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

const (
	DefaultBackoffBase = 500 * time.Millisecond

	DefaultBackoffMax = 5 * time.Minute
)

type backoff struct {
	base time.Duration

	max time.Duration

	mu sync.Mutex

	failures map[mcreconcile.Request]int
}

func newBackoff(base, max time.Duration) *backoff {
	if base <= 0 {
		base = DefaultBackoffBase
	}
	if max < base {
		max = base
	}
	return &backoff{base: base, max: max, failures: map[mcreconcile.Request]int{}}
}

func (b *backoff) delay(n int) time.Duration {
	d := b.base
	for i := 0; i < n && d < b.max; i++ {
		d *= 2
	}
	if d > b.max {
		d = b.max
	}
	return d
}

func (b *backoff) When(item mcreconcile.Request) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.failures[item]
	b.failures[item] = n + 1
	return b.delay(n)
}

func (b *backoff) Forget(item mcreconcile.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, item)
}

func (b *backoff) NumRequeues(item mcreconcile.Request) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures[item]
}

func (b *backoff) peek(item mcreconcile.Request) (int, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.failures[item]
	return n + 1, b.delay(n)
}

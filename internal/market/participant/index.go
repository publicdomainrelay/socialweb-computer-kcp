package participant

import (
	"context"
	"slices"
	"strings"
	"sync"
)

type Index[T any] struct {
	mu sync.Mutex

	items map[string]T

	changed chan struct{}
}

func NewIndex[T any]() *Index[T] {
	return &Index[T]{items: map[string]T{}, changed: make(chan struct{})}
}

func (x *Index[T]) Put(name string, value T) {
	x.mu.Lock()
	x.items[name] = value
	x.notifyLocked()
	x.mu.Unlock()
}

func (x *Index[T]) Remove(name string) {
	x.mu.Lock()
	if _, ok := x.items[name]; ok {
		delete(x.items, name)
		x.notifyLocked()
	}
	x.mu.Unlock()
}

func (x *Index[T]) Get(name string) (T, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	value, ok := x.items[name]
	return value, ok
}

func (x *Index[T]) List() []T {
	x.mu.Lock()
	names := make([]string, 0, len(x.items))
	for name := range x.items {
		names = append(names, name)
	}
	slices.SortFunc(names, strings.Compare)
	out := make([]T, 0, len(names))
	for _, name := range names {
		out = append(out, x.items[name])
	}
	x.mu.Unlock()
	return out
}

func (x *Index[T]) Find(match func(T) bool) (T, bool) {
	for _, value := range x.List() {
		if match(value) {
			return value, true
		}
	}
	var zero T
	return zero, false
}

func (x *Index[T]) Filter(match func(T) bool) []T {
	var out []T
	for _, value := range x.List() {
		if match(value) {
			out = append(out, value)
		}
	}
	return out
}

func (x *Index[T]) Await(ctx context.Context, done func() bool) error {
	for {
		x.mu.Lock()
		changed := x.changed
		x.mu.Unlock()
		if done() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (x *Index[T]) notifyLocked() {
	close(x.changed)
	x.changed = make(chan struct{})
}

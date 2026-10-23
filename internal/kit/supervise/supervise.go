// Package supervise owns goroutines: each runs under a name, a panic turns
// into an error that carries the name, and Stop cancels and joins them all.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"golang.org/x/sync/errgroup"
)

type Group struct {
	eg     *errgroup.Group
	ctx    context.Context
	cancel context.CancelFunc
}

// New returns a Group whose context is derived from parent. The first
// goroutine that returns a non-nil error cancels the context of all others.
func New(parent context.Context) *Group {
	ctx, cancel := context.WithCancel(parent)
	eg, ctx := errgroup.WithContext(ctx)
	return &Group{eg: eg, ctx: ctx, cancel: cancel}
}

func (g *Group) Context() context.Context { return g.ctx }

func (g *Group) Go(name string, fn func(context.Context) error) {
	g.eg.Go(func() error { return Run(g.ctx, name, fn) })
}

// Wait blocks until every goroutine returned and returns the first error.
func (g *Group) Wait() error {
	err := g.eg.Wait()
	g.cancel()
	return err
}

// Stop cancels the group context and waits. A context.Canceled error that
// the cancel caused is not returned.
func (g *Group) Stop() error {
	g.cancel()
	err := g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Run calls fn on the current goroutine, turns a panic into an error and
// prefixes a returned error with name.
func Run(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: panic: %v\n%s", name, r, debug.Stack())
		}
	}()
	if err := fn(ctx); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

type Backoff struct {
	Min     time.Duration
	Max     time.Duration
	OnError func(err error, delay time.Duration)
}

// GoRestart runs fn again after each return until the group context ends.
// The delay starts at Min, doubles up to Max, and resets when fn ran for
// longer than Max.
func (g *Group) GoRestart(name string, b Backoff, fn func(context.Context) error) {
	g.Go(name, func(ctx context.Context) error {
		delay := b.Min
		for {
			started := time.Now()
			err := Run(ctx, name, fn)
			if ctx.Err() != nil {
				return nil
			}
			if time.Since(started) > b.Max {
				delay = b.Min
			}
			if b.OnError != nil {
				b.OnError(err, delay)
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-t.C:
			}
			delay = min(delay*2, b.Max)
		}
	})
}

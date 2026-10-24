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

func New(parent context.Context) *Group {
	ctx, cancel := context.WithCancel(parent)
	eg, ctx := errgroup.WithContext(ctx)
	return &Group{eg: eg, ctx: ctx, cancel: cancel}
}

func (g *Group) Context() context.Context { return g.ctx }

func (g *Group) Go(name string, fn func(context.Context) error) {
	g.eg.Go(func() error { return Run(g.ctx, name, fn) })
}

func (g *Group) Wait() error {
	err := g.eg.Wait()
	g.cancel()
	return err
}

func (g *Group) Stop() error {
	g.cancel()
	err := g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

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

func Join(ctx context.Context, wait func()) error {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type Backoff struct {
	Min     time.Duration
	Max     time.Duration
	OnError func(err error, delay time.Duration)
}

func Restart(ctx context.Context, name string, b Backoff, fn func(context.Context) error) {
	delay := b.Min
	for {
		started := time.Now()
		err := Run(ctx, name, fn)
		if ctx.Err() != nil {
			return
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
			return
		case <-t.C:
		}
		delay = min(delay*2, b.Max)
	}
}

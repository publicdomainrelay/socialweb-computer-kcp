package component

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
)

type HandlerRegistry = bus.Bus

type Handlers = bus.Bus

type StreamHandler = bus.StreamHandler

const DefaultHandlerName = bus.DefaultHandlerName

var (
	ErrNoHandler       = bus.ErrNoHandler
	ErrNoStream        = bus.ErrNoStream
	ErrHandlersStopped = bus.ErrHandlersStopped
	ErrLinkDenied      = bus.ErrLinkDenied
)

func NewHandlerRegistry(ctx context.Context) *HandlerRegistry {
	return bus.New(ctx)
}

func NewHandlers(ctx context.Context) *HandlerRegistry {
	return bus.New(ctx)
}

func WithCaller(ctx context.Context, key Key) context.Context {
	return bus.WithCaller(ctx, key.Ref())
}

func CallerFrom(ctx context.Context) (Key, bool) {
	caller, ok := bus.CallerFrom(ctx)
	if !ok {
		return Key{}, false
	}
	return contract.KeyOf(caller.Ref), true
}

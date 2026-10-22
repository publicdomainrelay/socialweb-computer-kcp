package component

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const SystemKind = "System"

var ErrLinkDenied = errors.New("component: no ComponentLink allows this call")

type AccessLink struct {
	FromWorkspace string
	FromKind      string
	FromName      string
	ToWorkspace   string
	ToKind        string
	ToName        string
	Handlers      []string
}

type LinkDeniedError struct {
	From Key

	To Key

	Handler string
}

func (e *LinkDeniedError) Error() string {
	if e == nil {
		return ErrLinkDenied.Error()
	}
	return fmt.Sprintf("component: no ComponentLink allows %s/%s in %s to call %s/%s in %s handler %q",
		e.From.Kind, e.From.Name, e.From.Workspace, e.To.Kind, e.To.Name, e.To.Workspace, e.Handler)
}

func (e *LinkDeniedError) Unwrap() error {
	return ErrLinkDenied
}

type callerKey struct{}

func WithCaller(ctx context.Context, key Key) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callerKey{}, key)
}

func WithSystemCaller(ctx context.Context) context.Context {
	return WithCaller(ctx, Key{Kind: SystemKind})
}

func CallerFrom(ctx context.Context) (Key, bool) {
	if ctx == nil {
		return Key{}, false
	}
	key, ok := ctx.Value(callerKey{}).(Key)
	if !ok {
		return Key{}, false
	}
	return key, true
}

func SystemCaller(key Key) bool {
	return strings.EqualFold(strings.TrimSpace(key.Kind), SystemKind)
}

func AccessLinkAllows(link AccessLink, from, to Key, handler string) bool {
	if !matchesPattern(link.FromWorkspace, link.FromKind, link.FromName, from) {
		return false
	}
	if !matchesPattern(link.ToWorkspace, link.ToKind, link.ToName, to) {
		return false
	}
	if len(link.Handlers) == 0 {
		return true
	}
	handler = normalizeHandlerName(handler)
	for _, name := range link.Handlers {
		if normalizeHandlerName(name) == handler {
			return true
		}
	}
	return false
}

func matchesPattern(workspace, kind, name string, key Key) bool {
	if value := strings.TrimSpace(workspace); value != "" && value != key.Workspace {
		return false
	}
	if value := strings.TrimSpace(kind); value != "" && !strings.EqualFold(value, key.Kind) {
		return false
	}
	if value := strings.TrimSpace(name); value != "" && !strings.EqualFold(value, key.Name) {
		return false
	}
	return true
}

package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
)

const engagedSourceStart = "Starting EventSource"

type runtimeLogHandler struct {
	next slog.Handler
}

func newRuntimeLogHandler(next slog.Handler) slog.Handler {
	return runtimeLogHandler{next: next}
}

func (h runtimeLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h runtimeLogHandler) Handle(ctx context.Context, r slog.Record) error {
	level := r.Level
	if r.Message == engagedSourceStart && level == slog.LevelInfo {
		level = slog.LevelDebug
		if !h.next.Enabled(ctx, level) {
			return nil
		}
	}
	out := slog.NewRecord(r.Time, level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(encodableAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h runtimeLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return runtimeLogHandler{next: h.next.WithAttrs(encodableAttrs(attrs))}
}

func (h runtimeLogHandler) WithGroup(name string) slog.Handler {
	return runtimeLogHandler{next: h.next.WithGroup(name)}
}

func encodableAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = encodableAttr(a)
	}
	return out
}

func encodableAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindGroup:
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(encodableAttrs(a.Value.Group())...)}
	case slog.KindAny:
		v := a.Value.Any()
		switch reflect.ValueOf(v).Kind() {
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:
			return slog.String(a.Key, fmt.Sprintf("%T", v))
		}
	}
	return a
}

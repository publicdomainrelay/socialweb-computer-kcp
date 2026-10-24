package guestagent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

type pionLogs struct {
	log *slog.Logger
}

func (f pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{log: f.log.With("pion", scope)}
}

type pionLogger struct {
	log *slog.Logger
}

func (l pionLogger) emit(level slog.Level, msg string) {
	if l.log.Enabled(context.Background(), level) {
		l.log.Log(context.Background(), level, msg)
	}
}

func (l pionLogger) Trace(string) {}

func (l pionLogger) Tracef(string, ...any) {}

func (l pionLogger) Debug(string) {}

func (l pionLogger) Debugf(string, ...any) {}

func (l pionLogger) Info(msg string) { l.emit(slog.LevelDebug, msg) }

func (l pionLogger) Infof(format string, args ...any) {
	l.emit(slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l pionLogger) Warn(msg string) { l.emit(slog.LevelDebug, msg) }

func (l pionLogger) Warnf(format string, args ...any) {
	l.emit(slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l pionLogger) Error(msg string) { l.emit(slog.LevelWarn, msg) }

func (l pionLogger) Errorf(format string, args ...any) {
	l.emit(slog.LevelWarn, fmt.Sprintf(format, args...))
}

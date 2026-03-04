package daemon

import (
	"fmt"
	"log"
	"log/slog"
)

// toSlogLevel maps the daemon's LogLevel to slog.Level.
func toSlogLevel(l LogLevel) slog.Level {
	switch l {
	case LogLevelDebug:
		return slog.LevelDebug
	case LogLevelInfo:
		return slog.LevelInfo
	case LogLevelWarn:
		return slog.LevelWarn
	case LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// DaemonLogger provides structured logging for daemon components.
// It wraps *slog.Logger with a component name baked in.
type DaemonLogger struct {
	slogger *slog.Logger
}

// NewDaemonLogger creates a DaemonLogger for the given component.
// handler is the slog.Handler to use (e.g. slog.NewTextHandler).
func NewDaemonLogger(component string, handler slog.Handler) *DaemonLogger {
	return &DaemonLogger{
		slogger: slog.New(handler).With("component", component),
	}
}

// NewDaemonLoggerFromLegacy creates a DaemonLogger that bridges a legacy *log.Logger.
// This allows gradual migration: existing callers keep their *log.Logger and LogLevel,
// and DaemonLogger translates to slog internally.
func NewDaemonLoggerFromLegacy(component string, logger *log.Logger, minLevel LogLevel) *DaemonLogger {
	w := logger.Writer()
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: toSlogLevel(minLevel),
	})
	return NewDaemonLogger(component, handler)
}

// Logf logs a formatted message at the given level (migration shim for existing log() calls).
func (dl *DaemonLogger) Logf(level LogLevel, format string, args ...any) {
	dl.slogger.Log(nil, toSlogLevel(level), fmt.Sprintf(format, args...))
}

// Log logs a message with structured attributes.
func (dl *DaemonLogger) Log(level LogLevel, msg string, attrs ...slog.Attr) {
	dl.slogger.LogAttrs(nil, toSlogLevel(level), msg, attrs...)
}

// With returns a new DaemonLogger with additional default attributes.
func (dl *DaemonLogger) With(args ...any) *DaemonLogger {
	return &DaemonLogger{slogger: dl.slogger.With(args...)}
}

// Slog returns the underlying *slog.Logger for advanced use.
func (dl *DaemonLogger) Slog() *slog.Logger {
	return dl.slogger
}

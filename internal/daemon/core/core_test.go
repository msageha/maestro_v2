package core

import (
	"io"
	"log"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  LogLevel
	}{
		{"debug", LogLevelDebug},
		{"DEBUG", LogLevelDebug},
		{"Debug", LogLevelDebug},
		{"info", LogLevelInfo},
		{"INFO", LogLevelInfo},
		{"warn", LogLevelWarn},
		{"warning", LogLevelWarn},
		{"WARN", LogLevelWarn},
		{"WARNING", LogLevelWarn},
		{"error", LogLevelError},
		{"ERROR", LogLevelError},
		{"", LogLevelInfo},        // empty defaults to info
		{"unknown", LogLevelInfo}, // unknown defaults to info
		{"trace", LogLevelInfo},   // unsupported level defaults to info
		{"fatal", LogLevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := ParseLogLevel(tt.input)
			if got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestDaemonLogger_NilSafe(t *testing.T) {
	t.Parallel()
	// nil DaemonLogger should not panic
	var dl *DaemonLogger
	dl.Logf(LogLevelInfo, "test %s", "msg")
	dl.Log(LogLevelInfo, "test")
	w := dl.With("key", "val")
	if w != nil {
		t.Error("With on nil DaemonLogger should return nil")
	}
	s := dl.Slog()
	if s != nil {
		t.Error("Slog on nil DaemonLogger should return nil")
	}
}

func TestLogMixin_NilDL(t *testing.T) {
	t.Parallel()
	// LogMixin with nil DL should not panic
	m := &LogMixin{DL: nil}
	m.Log(LogLevelInfo, "test %s", "msg") // should be no-op
}

func TestNewDaemonLoggerFromLegacy(t *testing.T) {
	t.Parallel()
	logger := log.New(io.Discard, "", 0)
	dl := NewDaemonLoggerFromLegacy("test-component", logger, LogLevelDebug)
	if dl == nil {
		t.Fatal("NewDaemonLoggerFromLegacy returned nil")
	}
	// Should not panic
	dl.Logf(LogLevelInfo, "hello %s", "world")
	dl.Log(LogLevelDebug, "msg")
}

func TestToSlogLevel(t *testing.T) {
	t.Parallel()
	// Just verify the mapping doesn't panic for all known levels
	levels := []LogLevel{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError}
	for _, l := range levels {
		_ = toSlogLevel(l)
	}
	// Unknown level
	_ = toSlogLevel(LogLevel(99))
}

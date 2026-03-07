package core

import (
	"bytes"
	"log"
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  LogLevel
	}{
		{"debug", LogLevelDebug},
		{"DEBUG", LogLevelDebug},
		{"info", LogLevelInfo},
		{"INFO", LogLevelInfo},
		{"warn", LogLevelWarn},
		{"warning", LogLevelWarn},
		{"error", LogLevelError},
		{"ERROR", LogLevelError},
		{"unknown", LogLevelInfo},
		{"", LogLevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseLogLevel(tt.input)
			if got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestToSlogLevel(t *testing.T) {
	tests := []struct {
		input LogLevel
		want  slog.Level
	}{
		{LogLevelDebug, slog.LevelDebug},
		{LogLevelInfo, slog.LevelInfo},
		{LogLevelWarn, slog.LevelWarn},
		{LogLevelError, slog.LevelError},
		{LogLevel(99), slog.LevelInfo}, // default case
	}

	for _, tt := range tests {
		got := toSlogLevel(tt.input)
		if got != tt.want {
			t.Errorf("toSlogLevel(%v) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestNewDaemonLogger(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	dl := NewDaemonLogger("test-component", handler)

	dl.Logf(LogLevelInfo, "hello %s", "world")

	output := buf.String()
	if output == "" {
		t.Error("expected log output, got empty")
	}
}

func TestNewDaemonLoggerFromLegacy(t *testing.T) {
	var buf bytes.Buffer
	legacyLogger := log.New(&buf, "", 0)
	dl := NewDaemonLoggerFromLegacy("legacy", legacyLogger, LogLevelInfo)

	if dl == nil {
		t.Fatal("expected non-nil DaemonLogger")
	}
	if dl.Slog() == nil {
		t.Fatal("expected non-nil slog.Logger")
	}
}

func TestDaemonLogger_With(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	dl := NewDaemonLogger("test", handler)
	dl2 := dl.With("key", "value")

	if dl2 == nil {
		t.Fatal("expected non-nil DaemonLogger from With")
	}

	dl2.Logf(LogLevelInfo, "test message")
	output := buf.String()
	if output == "" {
		t.Error("expected log output from With logger")
	}
}

func TestRealClock_Now(t *testing.T) {
	c := RealClock{}
	now := c.Now()
	if now.IsZero() {
		t.Error("RealClock.Now() returned zero time")
	}
}

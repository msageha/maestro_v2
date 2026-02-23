package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestNewDaemon(t *testing.T) {
	var buf bytes.Buffer
	cfg := model.Config{
		Watcher: model.WatcherConfig{ScanIntervalSec: 5},
		Daemon:  model.DaemonConfig{ShutdownTimeoutSec: 10},
		Logging: model.LoggingConfig{Level: "debug"},
	}

	d, err := newDaemon("/tmp/test-maestro", cfg, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.maestroDir != "/tmp/test-maestro" {
		t.Errorf("maestroDir: got %q, want %q", d.maestroDir, "/tmp/test-maestro")
	}
	if d.logLevel != LogLevelDebug {
		t.Errorf("logLevel: got %d, want %d", d.logLevel, LogLevelDebug)
	}
}

func TestDaemonShutdownIdempotent(t *testing.T) {
	var buf bytes.Buffer
	cfg := model.Config{
		Watcher: model.WatcherConfig{ScanIntervalSec: 1},
		Daemon:  model.DaemonConfig{ShutdownTimeoutSec: 1},
	}

	d, err := newDaemon("/tmp/test-maestro-shutdown", cfg, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Create a ticker so Shutdown can stop it
	d.ticker = time.NewTicker(time.Hour)

	// Shutdown should be idempotent
	d.Shutdown()
	d.Shutdown() // second call should not panic
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected LogLevel
	}{
		{"debug", LogLevelDebug},
		{"DEBUG", LogLevelDebug},
		{"info", LogLevelInfo},
		{"warn", LogLevelWarn},
		{"warning", LogLevelWarn},
		{"error", LogLevelError},
		{"unknown", LogLevelInfo},
		{"", LogLevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseLogLevel(tt.input)
			if got != tt.expected {
				t.Errorf("parseLogLevel(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestDaemonLog(t *testing.T) {
	var buf bytes.Buffer
	cfg := model.Config{
		Logging: model.LoggingConfig{Level: "warn"},
	}

	d, err := newDaemon("", cfg, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Info should be filtered
	d.log(LogLevelInfo, "should not appear")
	if buf.Len() != 0 {
		t.Errorf("expected no output, got: %s", buf.String())
	}

	// Warn should pass
	d.log(LogLevelWarn, "warning message")
	if !bytes.Contains(buf.Bytes(), []byte("WARN")) {
		t.Errorf("expected WARN in output, got: %s", buf.String())
	}
}

func TestDaemonNew_CreatesLogDir(t *testing.T) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	if err := os.MkdirAll(maestroDir, 0755); err != nil {
		t.Fatalf("create maestro dir: %v", err)
	}

	cfg := model.Config{}
	d, err := New(maestroDir, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.logFile != nil {
		d.logFile.Close()
	}

	logDir := filepath.Join(maestroDir, "logs")
	if _, err := os.Stat(logDir); err != nil {
		t.Errorf("expected log dir to be created: %v", err)
	}
}

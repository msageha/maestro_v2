package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
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

func TestSelfWriteTracker_RecordAndConsume(t *testing.T) {
	tracker := newSelfWriteTracker()

	// Consume without Record returns false
	if tracker.Consume("/some/path.yaml") {
		t.Error("expected Consume to return false for unrecorded path")
	}

	// Record then Consume returns true
	tracker.Record("/some/path.yaml")
	if !tracker.Consume("/some/path.yaml") {
		t.Error("expected Consume to return true for recorded path")
	}

	// Second Consume returns false (consumed)
	if tracker.Consume("/some/path.yaml") {
		t.Error("expected Consume to return false after already consumed")
	}
}

func TestSelfWriteTracker_MultiplePaths(t *testing.T) {
	tracker := newSelfWriteTracker()

	tracker.Record("/a.yaml")
	tracker.Record("/b.yaml")

	if !tracker.Consume("/a.yaml") {
		t.Error("expected Consume to return true for /a.yaml")
	}
	if !tracker.Consume("/b.yaml") {
		t.Error("expected Consume to return true for /b.yaml")
	}
}

func TestSelfWriteTracker_Concurrent(t *testing.T) {
	tracker := newSelfWriteTracker()
	done := make(chan struct{})

	// Concurrent Record and Consume must not panic
	go func() {
		for i := 0; i < 100; i++ {
			tracker.Record("/path.yaml")
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		tracker.Consume("/path.yaml")
	}
	<-done
}

func TestSelfWriteTracker_StaleCleanupOnRecord(t *testing.T) {
	tracker := newSelfWriteTracker()

	// Inject stale entries by directly manipulating the map
	tracker.mu.Lock()
	tracker.paths["/stale1.yaml"] = time.Now().Add(-60 * time.Second) // 60s ago
	tracker.paths["/stale2.yaml"] = time.Now().Add(-45 * time.Second) // 45s ago
	tracker.paths["/fresh.yaml"] = time.Now()                         // just now
	tracker.mu.Unlock()

	if tracker.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", tracker.Len())
	}

	// Record triggers opportunistic cleanup of entries > 30s
	tracker.Record("/new.yaml")

	// Stale entries should be cleaned, fresh + new should remain
	if tracker.Len() != 2 {
		t.Errorf("expected 2 entries after stale cleanup, got %d", tracker.Len())
	}

	// Fresh entry should still be consumable
	if !tracker.Consume("/fresh.yaml") {
		t.Error("expected /fresh.yaml to still be consumable")
	}
	if !tracker.Consume("/new.yaml") {
		t.Error("expected /new.yaml to still be consumable")
	}
}

func TestSelfWriteTracker_StaleCleanupOnConsumeMiss(t *testing.T) {
	tracker := newSelfWriteTracker()

	// Inject stale entries
	tracker.mu.Lock()
	tracker.paths["/stale.yaml"] = time.Now().Add(-60 * time.Second)
	tracker.paths["/fresh.yaml"] = time.Now()
	tracker.mu.Unlock()

	if tracker.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", tracker.Len())
	}

	// Consume for a non-existent path triggers cleanup
	if tracker.Consume("/nonexistent.yaml") {
		t.Error("expected false for nonexistent path")
	}

	// Stale entry should be cleaned up
	if tracker.Len() != 1 {
		t.Errorf("expected 1 entry after Consume cleanup, got %d", tracker.Len())
	}

	// Fresh entry should still be consumable
	if !tracker.Consume("/fresh.yaml") {
		t.Error("expected /fresh.yaml to still be consumable")
	}
}

func TestSelfWriteTracker_StaleCleanupOnConsumeHit(t *testing.T) {
	tracker := newSelfWriteTracker()

	// Inject stale + target entries
	tracker.mu.Lock()
	tracker.paths["/stale.yaml"] = time.Now().Add(-60 * time.Second)
	tracker.paths["/target.yaml"] = time.Now()
	tracker.mu.Unlock()

	// Consume the target — should also clean up stale
	if !tracker.Consume("/target.yaml") {
		t.Error("expected true for target path")
	}

	// Stale entry should be cleaned
	if tracker.Len() != 0 {
		t.Errorf("expected 0 entries after consume+cleanup, got %d", tracker.Len())
	}
}

func TestSelfWriteTracker_ExpiredConsumeReturnsFalse(t *testing.T) {
	tracker := newSelfWriteTracker()

	// Inject an entry that is within the 30s stale threshold but beyond the 10s consume window
	tracker.mu.Lock()
	tracker.paths["/expired.yaml"] = time.Now().Add(-15 * time.Second)
	tracker.mu.Unlock()

	// Consume should return false since the entry is older than 10s
	if tracker.Consume("/expired.yaml") {
		t.Error("expected false for entry older than 10s consume window")
	}

	// Entry should be deleted after failed consume
	if tracker.Len() != 0 {
		t.Errorf("expected 0 entries after expired consume, got %d", tracker.Len())
	}
}

func TestNotifySelfWrite_PublishesEvent(t *testing.T) {
	d := newTestDaemon(t)
	d.eventBus = events.NewBus(10)
	defer d.eventBus.Close()

	received := make(chan events.Event, 1)
	unsub := d.eventBus.Subscribe(events.EventQueueWritten, func(e events.Event) {
		received <- e
	})
	defer unsub()

	queuePath := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	d.notifySelfWrite(queuePath, "command")

	// Verify self-write was recorded
	if !d.selfWrites.Consume(queuePath) {
		t.Error("expected self-write to be recorded")
	}

	// Verify event was published
	select {
	case e := <-received:
		if e.Type != events.EventQueueWritten {
			t.Errorf("expected EventQueueWritten, got %s", e.Type)
		}
		if file, ok := e.Data["file"].(string); !ok || file != "planner.yaml" {
			t.Errorf("expected file=planner.yaml, got %v", e.Data["file"])
		}
		if writeType, ok := e.Data["type"].(string); !ok || writeType != "command" {
			t.Errorf("expected type=command, got %v", e.Data["type"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for EventQueueWritten")
	}
}

func TestRecordSelfWrite_NoEvent(t *testing.T) {
	d := newTestDaemon(t)
	d.eventBus = events.NewBus(10)
	defer d.eventBus.Close()

	received := make(chan events.Event, 1)
	unsub := d.eventBus.Subscribe(events.EventQueueWritten, func(e events.Event) {
		received <- e
	})
	defer unsub()

	path := filepath.Join(d.maestroDir, "results", "worker1.yaml")
	d.recordSelfWrite(path)

	// Verify self-write was recorded
	if !d.selfWrites.Consume(path) {
		t.Error("expected self-write to be recorded")
	}

	// Verify NO event was published (recordSelfWrite does not publish)
	select {
	case <-received:
		t.Error("expected no EventQueueWritten for recordSelfWrite")
	case <-time.After(100 * time.Millisecond):
		// expected: no event
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

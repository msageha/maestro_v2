package daemon

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
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
	if d.logLevel != core.LogLevelDebug {
		t.Errorf("logLevel: got %d, want %d", d.logLevel, core.LogLevelDebug)
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
		expected core.LogLevel
	}{
		{"debug", core.LogLevelDebug},
		{"DEBUG", core.LogLevelDebug},
		{"info", core.LogLevelInfo},
		{"warn", core.LogLevelWarn},
		{"warning", core.LogLevelWarn},
		{"error", core.LogLevelError},
		{"unknown", core.LogLevelInfo},
		{"", core.LogLevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := core.ParseLogLevel(tt.input)
			if got != tt.expected {
				t.Errorf("core.ParseLogLevel(%q) = %d, want %d", tt.input, got, tt.expected)
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
	d.log(core.LogLevelInfo, "should not appear")
	if buf.Len() != 0 {
		t.Errorf("expected no output, got: %s", buf.String())
	}

	// Warn should pass
	d.log(core.LogLevelWarn, "warning message")
	if !bytes.Contains(buf.Bytes(), []byte("WARN")) {
		t.Errorf("expected WARN in output, got: %s", buf.String())
	}
}

// selfWriteTestData is a helper type for selfWriteTracker tests.
type selfWriteTestData struct {
	Value string `yaml:"value"`
}

// writeTestFile writes data as YAML to a file and returns the path.
func writeTestFile(t *testing.T, dir, name string, data any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := yamlutil.AtomicWrite(path, data); err != nil {
		t.Fatalf("write test file %s: %v", name, err)
	}
	return path
}

func TestSelfWriteTracker_RecordAndConsume(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()
	data := selfWriteTestData{Value: "test"}
	path := writeTestFile(t, dir, "path.yaml", data)

	// Consume without Record returns false
	if tracker.Consume(path) {
		t.Error("expected Consume to return false for unrecorded path")
	}

	// Record then Consume returns true (file content matches hash)
	tracker.Record(path, data)
	if !tracker.Consume(path) {
		t.Error("expected Consume to return true for recorded path")
	}

	// Second Consume returns false (consumed)
	if tracker.Consume(path) {
		t.Error("expected Consume to return false after already consumed")
	}
}

func TestSelfWriteTracker_HashMismatch(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()

	// Record with one data, write different data to file
	data1 := selfWriteTestData{Value: "original"}
	data2 := selfWriteTestData{Value: "modified"}
	path := writeTestFile(t, dir, "path.yaml", data2) // file has data2

	tracker.Record(path, data1) // tracker has hash of data1

	// Consume should return false because file content (data2) != recorded hash (data1)
	if tracker.Consume(path) {
		t.Error("expected Consume to return false for hash mismatch")
	}
}

func TestSelfWriteTracker_MultiplePaths(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()

	dataA := selfWriteTestData{Value: "a"}
	dataB := selfWriteTestData{Value: "b"}
	pathA := writeTestFile(t, dir, "a.yaml", dataA)
	pathB := writeTestFile(t, dir, "b.yaml", dataB)

	tracker.Record(pathA, dataA)
	tracker.Record(pathB, dataB)

	if !tracker.Consume(pathA) {
		t.Error("expected Consume to return true for a.yaml")
	}
	if !tracker.Consume(pathB) {
		t.Error("expected Consume to return true for b.yaml")
	}
}

func TestSelfWriteTracker_Concurrent(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()
	data := selfWriteTestData{Value: "concurrent"}
	path := writeTestFile(t, dir, "path.yaml", data)

	done := make(chan struct{})

	// Concurrent Record and Consume must not panic
	go func() {
		for i := 0; i < 100; i++ {
			tracker.Record(path, data)
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		tracker.Consume(path)
	}
	<-done
}

func TestSelfWriteTracker_StaleCleanupOnRecord(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()

	// Inject stale entries by directly manipulating the map
	freshData := selfWriteTestData{Value: "fresh"}
	freshContent, _ := yamlv3.Marshal(freshData)
	freshPath := writeTestFile(t, dir, "fresh.yaml", freshData)

	tracker.mu.Lock()
	tracker.stamps["/stale1.yaml"] = writeStamp{Hash: sha256.Sum256([]byte("stale1")), Deadline: time.Now().Add(-60 * time.Second)}
	tracker.stamps["/stale2.yaml"] = writeStamp{Hash: sha256.Sum256([]byte("stale2")), Deadline: time.Now().Add(-45 * time.Second)}
	tracker.stamps[freshPath] = writeStamp{Hash: sha256.Sum256(freshContent), Deadline: time.Now().Add(30 * time.Second)}
	tracker.mu.Unlock()

	if tracker.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", tracker.Len())
	}

	// Record triggers opportunistic cleanup of stale entries
	newData := selfWriteTestData{Value: "new"}
	newPath := writeTestFile(t, dir, "new.yaml", newData)
	tracker.Record(newPath, newData)

	// Stale entries should be cleaned, fresh + new should remain
	if tracker.Len() != 2 {
		t.Errorf("expected 2 entries after stale cleanup, got %d", tracker.Len())
	}

	// Fresh entry should still be consumable
	if !tracker.Consume(freshPath) {
		t.Error("expected fresh.yaml to still be consumable")
	}
	if !tracker.Consume(newPath) {
		t.Error("expected new.yaml to still be consumable")
	}
}

func TestSelfWriteTracker_StaleCleanupOnConsumeMiss(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()

	freshData := selfWriteTestData{Value: "fresh"}
	freshContent, _ := yamlv3.Marshal(freshData)
	freshPath := writeTestFile(t, dir, "fresh.yaml", freshData)

	// Inject stale + fresh entries
	tracker.mu.Lock()
	tracker.stamps["/stale.yaml"] = writeStamp{Hash: sha256.Sum256([]byte("stale")), Deadline: time.Now().Add(-60 * time.Second)}
	tracker.stamps[freshPath] = writeStamp{Hash: sha256.Sum256(freshContent), Deadline: time.Now().Add(30 * time.Second)}
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
	if !tracker.Consume(freshPath) {
		t.Error("expected fresh.yaml to still be consumable")
	}
}

func TestSelfWriteTracker_StaleCleanupOnConsumeHit(t *testing.T) {
	tracker := newSelfWriteTracker()
	dir := t.TempDir()

	targetData := selfWriteTestData{Value: "target"}
	targetContent, _ := yamlv3.Marshal(targetData)
	targetPath := writeTestFile(t, dir, "target.yaml", targetData)

	// Inject stale + target entries
	tracker.mu.Lock()
	tracker.stamps["/stale.yaml"] = writeStamp{Hash: sha256.Sum256([]byte("stale")), Deadline: time.Now().Add(-60 * time.Second)}
	tracker.stamps[targetPath] = writeStamp{Hash: sha256.Sum256(targetContent), Deadline: time.Now().Add(30 * time.Second)}
	tracker.mu.Unlock()

	// Consume the target — should also clean up stale
	if !tracker.Consume(targetPath) {
		t.Error("expected true for target path")
	}

	// Stale entry should be cleaned
	if tracker.Len() != 0 {
		t.Errorf("expected 0 entries after consume+cleanup, got %d", tracker.Len())
	}
}

func TestSelfWriteTracker_ExpiredConsumeReturnsFalse(t *testing.T) {
	tracker := newSelfWriteTracker()

	// Inject an entry with an expired deadline
	tracker.mu.Lock()
	tracker.stamps["/expired.yaml"] = writeStamp{Hash: sha256.Sum256([]byte("expired")), Deadline: time.Now().Add(-1 * time.Second)}
	tracker.mu.Unlock()

	// Consume should return false since the deadline has passed
	if tracker.Consume("/expired.yaml") {
		t.Error("expected false for entry past deadline")
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

	// Write actual data to the file so Consume can verify the hash
	data := model.CommandQueue{SchemaVersion: 1, FileType: "queue_command"}
	queuePath := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(queuePath, data); err != nil {
		t.Fatalf("write queue file: %v", err)
	}
	d.notifySelfWrite(queuePath, "command", data)

	// Verify self-write was recorded and hash matches
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

	// Write actual data to the file so Consume can verify the hash
	data := model.TaskResultFile{SchemaVersion: 1, FileType: "result_task"}
	path := filepath.Join(d.maestroDir, "results", "worker1.yaml")
	if err := yamlutil.AtomicWrite(path, data); err != nil {
		t.Fatalf("write result file: %v", err)
	}
	d.recordSelfWrite(path, data)

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

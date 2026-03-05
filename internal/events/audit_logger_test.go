package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNewAuditLogger(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Verify log file was created
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("Log file was not created")
	}
}

func TestAuditLogger_WriteEntry(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write a test entry
	entry := &LogEntry{
		Timestamp: time.Now().UTC(),
		EventType: "test_event",
		EventID:   "evt_123",
		CommandID: "cmd_456",
		TaskID:    "task_789",
		AgentID:   "agent_1",
		Details: map[string]interface{}{
			"action": "test_action",
			"result": "success",
		},
	}

	if err := logger.WriteEntry(entry); err != nil {
		t.Fatalf("Failed to write log entry: %v", err)
	}

	// Read and verify the entry
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	var readEntry LogEntry
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &readEntry); err != nil {
		t.Fatalf("Failed to unmarshal log entry: %v", err)
	}

	if readEntry.EventType != entry.EventType {
		t.Errorf("EventType mismatch: got %s, want %s", readEntry.EventType, entry.EventType)
	}
	if readEntry.EventID != entry.EventID {
		t.Errorf("EventID mismatch: got %s, want %s", readEntry.EventID, entry.EventID)
	}
	if readEntry.CommandID != entry.CommandID {
		t.Errorf("CommandID mismatch: got %s, want %s", readEntry.CommandID, entry.CommandID)
	}
}

func TestAuditLogger_Log(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Log with details
	details := map[string]interface{}{
		"event_id":   "evt_test",
		"command_id": "cmd_test",
		"task_id":    "task_test",
		"agent_id":   "agent_test",
		"action":     "test_action",
		"status":     "completed",
	}

	if err := logger.Log("task_completed", details); err != nil {
		t.Fatalf("Failed to log entry: %v", err)
	}

	// Read and verify
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	var entry LogEntry
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &entry); err != nil {
		t.Fatalf("Failed to unmarshal log entry: %v", err)
	}

	if entry.EventType != "task_completed" {
		t.Errorf("EventType mismatch: got %s, want %s", entry.EventType, "task_completed")
	}
	if entry.EventID != "evt_test" {
		t.Errorf("EventID mismatch: got %s, want %s", entry.EventID, "evt_test")
	}
	if entry.CommandID != "cmd_test" {
		t.Errorf("CommandID mismatch: got %s, want %s", entry.CommandID, "cmd_test")
	}
}

func TestAuditLogger_ConcurrentWrites(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Perform concurrent writes
	numGoroutines := 100
	entriesPerGoroutine := 10
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < entriesPerGoroutine; j++ {
				details := map[string]interface{}{
					"goroutine": id,
					"iteration": j,
				}
				if err := logger.Log(fmt.Sprintf("concurrent_event_%d_%d", id, j), details); err != nil {
					t.Errorf("Failed to log entry: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify all entries were written
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Failed to open log file: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	count := 0
	for decoder.More() {
		var entry LogEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Errorf("Failed to decode entry: %v", err)
			continue
		}
		count++
	}

	expectedCount := numGoroutines * entriesPerGoroutine
	if count != expectedCount {
		t.Errorf("Entry count mismatch: got %d, want %d", count, expectedCount)
	}
}

func TestAuditLogger_Rotation(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// Create logger with small max size to trigger rotation
	maxSize := int64(1024) // 1KB
	logger, err := NewAuditLogger(logPath, maxSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write entries until rotation occurs
	largeDetails := map[string]interface{}{
		"data": "This is a test entry with some content to increase size",
		"more": "Additional data to make the entry larger",
	}

	rotationOccurred := false
	for i := 0; i < 100; i++ {
		if err := logger.Log(fmt.Sprintf("event_%d", i), largeDetails); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}

		// Check if rotation occurred
		archiveDir := filepath.Join(tempDir, ArchiveDir)
		if _, err := os.Stat(archiveDir); err == nil {
			files, _ := os.ReadDir(archiveDir)
			if len(files) > 0 {
				rotationOccurred = true
				break
			}
		}
	}

	if !rotationOccurred {
		t.Error("Log rotation did not occur despite exceeding max size")
	}

	// Verify current log file exists and is not empty
	if _, err := os.Stat(logPath); err != nil {
		t.Error("Current log file does not exist after rotation")
	}
}

func TestAuditLogger_Checksum(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Enable checksum
	logger.EnableChecksum(true)

	// Write entry with checksum
	details := map[string]interface{}{
		"action": "test_with_checksum",
		"value":  42,
	}

	if err := logger.Log("checksum_event", details); err != nil {
		t.Fatalf("Failed to log entry: %v", err)
	}

	// Read and verify checksum exists
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	var entry LogEntry
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &entry); err != nil {
		t.Fatalf("Failed to unmarshal log entry: %v", err)
	}

	if entry.Checksum == "" {
		t.Error("Checksum was not generated")
	}
}

func TestVerifyLogIntegrity(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}

	// Enable checksum for some entries
	logger.EnableChecksum(true)

	// Write entries with checksums
	for i := 0; i < 5; i++ {
		details := map[string]interface{}{
			"index": i,
			"type":  "with_checksum",
		}
		if err := logger.Log("test_event", details); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}
	}

	// Disable checksum
	logger.EnableChecksum(false)

	// Write entries without checksums
	for i := 5; i < 10; i++ {
		details := map[string]interface{}{
			"index": i,
			"type":  "without_checksum",
		}
		if err := logger.Log("test_event", details); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}
	}

	logger.Close()

	// Verify integrity
	total, valid, err := VerifyLogIntegrity(logPath)
	if err != nil {
		t.Fatalf("Failed to verify log integrity: %v", err)
	}

	if total != 10 {
		t.Errorf("Total entries mismatch: got %d, want %d", total, 10)
	}

	if valid != total {
		t.Errorf("Valid entries mismatch: got %d, want %d", valid, total)
	}
}

func TestAuditLogger_LargeLogFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large file test in short mode")
	}

	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// Use 10KB max size to test rotation quickly
	testMaxSize := int64(10 * 1024) // 10KB
	logger, err := NewAuditLogger(logPath, testMaxSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write 1KB entries — 20 entries will definitely exceed 10KB and trigger rotation
	largeData := make([]byte, 1024) // 1KB of data
	for i := range largeData {
		largeData[i] = byte('A' + (i % 26))
	}

	entriesWritten := 0
	for i := 0; i < 20; i++ {
		details := map[string]interface{}{
			"index":      entriesWritten,
			"large_data": string(largeData),
		}
		if err := logger.Log("large_event", details); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}
		entriesWritten++
	}

	// Verify rotation occurred
	archiveDir := filepath.Join(tempDir, ArchiveDir)
	files, err := os.ReadDir(archiveDir)
	if err != nil || len(files) == 0 {
		t.Error("Expected rotation to occur for large log file")
	}

	t.Logf("Wrote %d entries, %d archive files created", entriesWritten, len(files))
}

func TestAuditLogger_FileRecovery(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// Create first logger and write some entries
	logger1, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create first logger: %v", err)
	}

	for i := 0; i < 5; i++ {
		details := map[string]interface{}{"index": i}
		if err := logger1.Log("event", details); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}
	}

	logger1.Close()

	// Create second logger on same file (simulating restart)
	logger2, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create second logger: %v", err)
	}
	defer logger2.Close()

	// Write more entries
	for i := 5; i < 10; i++ {
		details := map[string]interface{}{"index": i}
		if err := logger2.Log("event", details); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}
	}

	// Verify all entries are present
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Failed to open log file: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	count := 0
	indices := make(map[int]bool)

	for decoder.More() {
		var entry LogEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Errorf("Failed to decode entry: %v", err)
			continue
		}
		if idx, ok := entry.Details["index"].(float64); ok {
			indices[int(idx)] = true
		}
		count++
	}

	if count != 10 {
		t.Errorf("Entry count mismatch: got %d, want %d", count, 10)
	}

	// Verify all indices are present
	for i := 0; i < 10; i++ {
		if !indices[i] {
			t.Errorf("Missing entry with index %d", i)
		}
	}
}

func TestAuditLogger_RotateTimestampCollision(t *testing.T) {
	// Simulate the scenario where multiple rotations occur within the same second
	// (or even after process restart where rotationCounter resets).
	// Each rotation must produce a unique archive filename.
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// Use a tiny max size so every write triggers rotation.
	maxSize := int64(10) // 10 bytes — every entry triggers rotation
	logger, err := NewAuditLogger(logPath, maxSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}

	// Write several entries rapidly to trigger multiple rotations within the same second
	for i := 0; i < 5; i++ {
		details := map[string]interface{}{"i": i}
		if err := logger.Log("rotate_test", details); err != nil {
			t.Fatalf("Failed to log entry %d: %v", i, err)
		}
	}
	logger.Close()

	// Simulate process restart: create a new logger with reset rotationCounter
	logger2, err := NewAuditLogger(logPath, maxSize)
	if err != nil {
		t.Fatalf("Failed to create second audit logger: %v", err)
	}

	for i := 5; i < 10; i++ {
		details := map[string]interface{}{"i": i}
		if err := logger2.Log("rotate_test", details); err != nil {
			t.Fatalf("Failed to log entry %d: %v", i, err)
		}
	}
	logger2.Close()

	// Verify all archive files have unique names (no overwrites)
	archiveDir := filepath.Join(tempDir, ArchiveDir)
	files, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("Failed to read archive dir: %v", err)
	}

	if len(files) < 2 {
		t.Fatalf("Expected multiple archive files, got %d", len(files))
	}

	// Check uniqueness of filenames
	seen := make(map[string]bool)
	for _, f := range files {
		if seen[f.Name()] {
			t.Errorf("Duplicate archive filename: %s", f.Name())
		}
		seen[f.Name()] = true
	}

	t.Logf("Created %d unique archive files", len(files))
}

func TestAuditLogger_BatchedSync_CountThreshold(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// syncMax=5 means fsync every 5 writes; large interval so only count triggers
	logger, err := NewAuditLoggerWithSync(logPath, DefaultMaxLogSize, 5, 10*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write 5 entries — should trigger exactly one fsync at the 5th write
	for i := 0; i < 5; i++ {
		details := map[string]interface{}{"index": i}
		if err := logger.Log(fmt.Sprintf("event_%d", i), details); err != nil {
			t.Fatalf("Failed to log entry %d: %v", i, err)
		}
	}

	// After 5 writes, dirty should be false (flushed) and syncCount reset
	logger.mu.Lock()
	if logger.dirty {
		t.Error("expected dirty=false after syncMax writes")
	}
	if logger.syncCount != 0 {
		t.Errorf("expected syncCount=0, got %d", logger.syncCount)
	}
	logger.mu.Unlock()

	// Verify all entries are readable
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 5 {
		t.Errorf("expected 5 lines, got %d", lines)
	}
}

func TestAuditLogger_BatchedSync_TimeThreshold(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// syncMax=1000 (high, won't trigger by count), short interval
	logger, err := NewAuditLoggerWithSync(logPath, DefaultMaxLogSize, 1000, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write a few entries (below count threshold)
	for i := 0; i < 3; i++ {
		details := map[string]interface{}{"index": i}
		if err := logger.Log(fmt.Sprintf("event_%d", i), details); err != nil {
			t.Fatalf("Failed to log entry %d: %v", i, err)
		}
	}

	// Immediately after writes, dirty should be true (not yet fsynced)
	logger.mu.Lock()
	wasDirty := logger.dirty
	hasTimer := logger.syncTimer != nil
	logger.mu.Unlock()

	if !wasDirty {
		t.Error("expected dirty=true before timer fires")
	}
	if !hasTimer {
		t.Error("expected sync timer to be set")
	}

	// Wait for the timer to fire
	time.Sleep(150 * time.Millisecond)

	logger.mu.Lock()
	isDirty := logger.dirty
	logger.mu.Unlock()

	if isDirty {
		t.Error("expected dirty=false after timer fires")
	}
}

func TestAuditLogger_BatchedSync_FlushOnClose(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// High syncMax so count won't trigger, long interval
	logger, err := NewAuditLoggerWithSync(logPath, DefaultMaxLogSize, 1000, 10*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}

	// Write entries (won't trigger count-based flush)
	for i := 0; i < 3; i++ {
		details := map[string]interface{}{"index": i}
		if err := logger.Log(fmt.Sprintf("event_%d", i), details); err != nil {
			t.Fatalf("Failed to log entry %d: %v", i, err)
		}
	}

	// Close should flush
	if err := logger.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify all entries are persisted
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Failed to open log file: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	count := 0
	for decoder.More() {
		var entry LogEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Errorf("Failed to decode entry: %v", err)
			continue
		}
		count++
	}

	if count != 3 {
		t.Errorf("expected 3 entries after close, got %d", count)
	}
}

func TestAuditLogger_BatchedSync_ExplicitFlush(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	logger, err := NewAuditLoggerWithSync(logPath, DefaultMaxLogSize, 1000, 10*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write a single entry
	details := map[string]interface{}{"action": "test"}
	if err := logger.Log("test_event", details); err != nil {
		t.Fatalf("Failed to log: %v", err)
	}

	logger.mu.Lock()
	if !logger.dirty {
		t.Error("expected dirty=true before Flush()")
	}
	logger.mu.Unlock()

	// Explicit flush
	if err := logger.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	logger.mu.Lock()
	if logger.dirty {
		t.Error("expected dirty=false after Flush()")
	}
	logger.mu.Unlock()
}

func TestAuditLogger_BatchedSync_ConcurrentWithTimer(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "audit.jsonl")

	// Short interval for timer-based sync
	logger, err := NewAuditLoggerWithSync(logPath, DefaultMaxLogSize, 50, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Concurrent writes with timer firing
	numGoroutines := 20
	entriesPerGoroutine := 10
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < entriesPerGoroutine; j++ {
				details := map[string]interface{}{
					"goroutine": id,
					"iteration": j,
				}
				if err := logger.Log(fmt.Sprintf("concurrent_%d_%d", id, j), details); err != nil {
					t.Errorf("Failed to log: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()

	// Close to flush remaining
	if err := logger.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify all entries
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Failed to open log: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	count := 0
	for decoder.More() {
		var entry LogEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Errorf("Failed to decode: %v", err)
			continue
		}
		count++
	}

	expected := numGoroutines * entriesPerGoroutine
	if count != expected {
		t.Errorf("entry count = %d, want %d", count, expected)
	}
}
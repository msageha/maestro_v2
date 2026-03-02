package events

import (
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
	if err := json.Unmarshal(data[:len(data)-1], &readEntry); err != nil {
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
	if err := json.Unmarshal(data[:len(data)-1], &entry); err != nil {
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
	if err := json.Unmarshal(data[:len(data)-1], &entry); err != nil {
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

	// Use default 100MB max size
	logger, err := NewAuditLogger(logPath, DefaultMaxLogSize)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Write entries until we approach 100MB
	largeData := make([]byte, 1024) // 1KB of data
	for i := range largeData {
		largeData[i] = byte('A' + (i % 26))
	}

	entriesWritten := 0
	targetSize := int64(100 * 1024 * 1024) // 100MB

	for logger.GetCurrentSize() < targetSize-2048 {
		details := map[string]interface{}{
			"index":      entriesWritten,
			"large_data": string(largeData),
		}
		if err := logger.Log("large_event", details); err != nil {
			t.Fatalf("Failed to log entry: %v", err)
		}
		entriesWritten++
	}

	// Write one more entry to trigger rotation
	if err := logger.Log("trigger_rotation", map[string]interface{}{"final": true}); err != nil {
		t.Fatalf("Failed to log final entry: %v", err)
	}

	// Verify rotation occurred
	archiveDir := filepath.Join(tempDir, ArchiveDir)
	files, err := os.ReadDir(archiveDir)
	if err != nil || len(files) == 0 {
		t.Error("Expected rotation to occur for 100MB file")
	}

	t.Logf("Wrote %d entries before rotation", entriesWritten)
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
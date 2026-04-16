package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
)

func TestTraceWriter_HandleEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	defer tw.Close()

	ev := events.Event{
		Type:      events.EventTaskStarted,
		Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Data: map[string]interface{}{
			"task_id":    "t1",
			"command_id": "c1",
			"agent_id":   "worker1",
		},
	}
	tw.HandleEvent(ev)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}

	var entry events.LogEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if entry.EventType != string(events.EventTaskStarted) {
		t.Errorf("event_type = %q, want %q", entry.EventType, events.EventTaskStarted)
	}
	if entry.TaskID != "t1" {
		t.Errorf("task_id = %q, want %q", entry.TaskID, "t1")
	}
	if entry.CommandID != "c1" {
		t.Errorf("command_id = %q, want %q", entry.CommandID, "c1")
	}
	if entry.AgentID != "worker1" {
		t.Errorf("agent_id = %q, want %q", entry.AgentID, "worker1")
	}
}

func TestTraceWriter_MultipleEvents_JSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	defer tw.Close()

	for i, etype := range []events.EventType{
		events.EventTaskStarted,
		events.EventTaskCompleted,
		events.EventPhaseTransition,
	} {
		tw.HandleEvent(events.Event{
			Type:      etype,
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"task_id": "t" + string(rune('1'+i)),
			},
		})
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lines int
	for scanner.Scan() {
		var entry events.LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Errorf("line %d: unmarshal: %v", lines+1, err)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if lines != 3 {
		t.Errorf("got %d lines, want 3", lines)
	}
}

func TestTraceWriter_CloseRejectsWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// HandleEvent after Close should not panic or write.
	tw.HandleEvent(events.Event{
		Type:      events.EventTaskStarted,
		Timestamp: time.Now(),
		Data:      map[string]interface{}{"task_id": "t1"},
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file after Close, got %d bytes", len(data))
	}
}

func TestTraceWriter_DoubleCloseNoPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close should be a no-op.
	if err := tw.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewTraceWriter_OpenError(t *testing.T) {
	// Use a path inside a non-existent directory to trigger an error.
	path := filepath.Join(t.TempDir(), "no-such-dir", "sub", "trace.jsonl")
	_, err := NewTraceWriter(path)
	if err == nil {
		t.Fatal("expected error for non-existent directory, got nil")
	}
}

package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/msageha/maestro_v2/internal/events"
)

// TraceWriter subscribes to EventBus events and persists them as JSONL
// (one JSON object per line) to a file. It is safe for concurrent use.
type TraceWriter struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	closed bool
}

// NewTraceWriter opens (or creates) path in append mode and returns a
// ready-to-use TraceWriter. The caller must call Close when done.
func NewTraceWriter(path string) (*TraceWriter, error) {
	// path is provided by the daemon initializer from a controlled application directory.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // controlled path
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	return &TraceWriter{path: path, file: f}, nil
}

// HandleEvent serializes the event as a single JSON line and writes it to
// the trace file. It satisfies the events.Bus subscriber signature.
func (tw *TraceWriter) HandleEvent(event events.Event) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.closed {
		return
	}

	entry := events.LogEntry{
		Timestamp: event.Timestamp,
		EventType: string(event.Type),
		CommandID: stringFromData(event.Data, "command_id"),
		TaskID:    stringFromData(event.Data, "task_id"),
		AgentID:   stringFromData(event.Data, "agent_id"),
		Details:   event.Data,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		slog.Warn("trace_writer: marshal event failed", "error", err)
		return
	}
	data = append(data, '\n')
	if _, err := tw.file.Write(data); err != nil {
		slog.Warn("trace_writer: write event failed", "error", err)
	}
}

// Close flushes and closes the underlying file. After Close, HandleEvent
// silently drops events.
func (tw *TraceWriter) Close() error {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.closed {
		return nil
	}
	tw.closed = true
	return tw.file.Close()
}

// stringFromData extracts a string value from an event data map.
// Returns "" if the key is missing or the value is not a string.
func stringFromData(data map[string]interface{}, key string) string {
	if data == nil {
		return ""
	}
	v, ok := data[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

package events

import "time"

// LogEntry represents a single audit log entry
type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	EventType string                 `json:"event_type"`
	EventID   string                 `json:"event_id,omitempty"`
	CommandID string                 `json:"command_id,omitempty"`
	TaskID    string                 `json:"task_id,omitempty"`
	AgentID   string                 `json:"agent_id,omitempty"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Checksum  string                 `json:"checksum,omitempty"`
}

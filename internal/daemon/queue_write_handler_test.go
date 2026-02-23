package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	for _, sub := range []string{
		"queue", "results", "state/commands", "logs",
	} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create dir %s: %v", sub, err)
		}
	}

	var buf bytes.Buffer
	cfg := model.Config{
		Watcher: model.WatcherConfig{ScanIntervalSec: 60},
		Logging: model.LoggingConfig{Level: "error"},
		Limits: model.LimitsConfig{
			MaxPendingCommands:       10,
			MaxPendingTasksPerWorker: 10,
			MaxEntryContentBytes:     1024 * 1024,
			MaxYAMLFileBytes:         10 * 1024 * 1024,
		},
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{Count: 2},
		},
	}

	d, err := newDaemon(maestroDir, cfg, &buf, nil)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}

	// Initialize worker queue files
	for i := 1; i <= cfg.Agents.Workers.Count; i++ {
		tq := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task"}
		data, _ := yamlv3.Marshal(tq)
		path := filepath.Join(maestroDir, "queue", workerQueueFile(i))
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("write worker queue: %v", err)
		}
	}

	return d
}

func workerQueueFile(index int) string {
	return "worker" + itoa(index) + ".yaml"
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func makeQueueWriteRequest(t *testing.T, params any) *uds.Request {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return &uds.Request{
		ProtocolVersion: 1,
		Command:         "queue_write",
		Params:          data,
	}
}

func TestQueueWriteCommand_Basic(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "implement authentication",
	})

	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["id"] == "" {
		t.Error("expected non-empty id")
	}

	// Verify file was written
	path := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if len(cq.Commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(cq.Commands))
	}
	if cq.Commands[0].Content != "implement authentication" {
		t.Errorf("content = %q, want %q", cq.Commands[0].Content, "implement authentication")
	}
	if cq.Commands[0].Status != model.StatusPending {
		t.Errorf("status = %q, want %q", cq.Commands[0].Status, model.StatusPending)
	}
	if cq.Commands[0].Priority != 100 {
		t.Errorf("priority = %d, want 100 (default)", cq.Commands[0].Priority)
	}
}

func TestQueueWriteCommand_Backpressure(t *testing.T) {
	d := newTestDaemon(t)
	d.config.Limits.MaxPendingCommands = 2

	// Write 2 commands
	for i := 0; i < 2; i++ {
		req := makeQueueWriteRequest(t, QueueWriteParams{
			Type:    "command",
			Content: "cmd",
		})
		resp := d.handleQueueWrite(req)
		if !resp.Success {
			t.Fatalf("write %d: expected success, got error: %v", i, resp.Error)
		}
	}

	// 3rd should be rejected
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "cmd3",
	})
	resp := d.handleQueueWrite(req)
	if resp.Success {
		t.Fatal("expected backpressure error")
	}
	if resp.Error.Code != uds.ErrCodeBackpressure {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeBackpressure)
	}
}

func TestQueueWriteCommand_BackpressureCountsPendingOnly(t *testing.T) {
	d := newTestDaemon(t)
	d.config.Limits.MaxPendingCommands = 1

	// Write 1 pending command
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "cmd1",
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp.Error)
	}

	// Manually set it to in_progress (non-pending)
	path := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	data, _ := os.ReadFile(path)
	var cq model.CommandQueue
	yamlv3.Unmarshal(data, &cq)
	cq.Commands[0].Status = model.StatusInProgress
	yamlutil.AtomicWrite(path, cq)

	// Write another — should succeed because only pending counts for backpressure
	req2 := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "cmd2",
	})
	resp2 := d.handleQueueWrite(req2)
	if !resp2.Success {
		t.Fatalf("write 2: expected success (in_progress doesn't count), got error: %v", resp2.Error)
	}
}

func TestQueueWriteCommand_ContentSizeLimit(t *testing.T) {
	d := newTestDaemon(t)
	d.config.Limits.MaxEntryContentBytes = 10

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "this content exceeds the 10 byte limit",
	})
	resp := d.handleQueueWrite(req)
	if resp.Success {
		t.Fatal("expected validation error for oversized content")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestQueueWriteCommand_MissingContent(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type: "command",
	})
	resp := d.handleQueueWrite(req)
	if resp.Success {
		t.Fatal("expected validation error for missing content")
	}
}

func TestQueueWriteTask_Basic(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:               "task",
		CommandID:          "cmd_0000000001_abcdef01",
		Content:            "implement login page",
		Purpose:            "create login UI",
		AcceptanceCriteria: "login form renders correctly",
		BloomLevel:         3,
		Target:             "worker1",
	})

	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["id"] == "" {
		t.Error("expected non-empty id")
	}

	// Verify file
	path := filepath.Join(d.maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if len(tq.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tq.Tasks))
	}
	if tq.Tasks[0].Purpose != "create login UI" {
		t.Errorf("purpose = %q, want %q", tq.Tasks[0].Purpose, "create login UI")
	}
	if tq.Tasks[0].BloomLevel != 3 {
		t.Errorf("bloom_level = %d, want %d", tq.Tasks[0].BloomLevel, 3)
	}
	if tq.Tasks[0].Priority != 100 {
		t.Errorf("priority = %d, want 100 (default)", tq.Tasks[0].Priority)
	}
}

func TestQueueWriteTask_Backpressure(t *testing.T) {
	d := newTestDaemon(t)
	d.config.Limits.MaxPendingTasksPerWorker = 1

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:               "task",
		CommandID:          "cmd_0000000001_abcdef01",
		Content:            "task1",
		Purpose:            "purpose1",
		AcceptanceCriteria: "criteria1",
		BloomLevel:         2,
		Target:             "worker1",
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp.Error)
	}

	// 2nd should be rejected
	req2 := makeQueueWriteRequest(t, QueueWriteParams{
		Type:               "task",
		CommandID:          "cmd_0000000001_abcdef01",
		Content:            "task2",
		Purpose:            "purpose2",
		AcceptanceCriteria: "criteria2",
		BloomLevel:         2,
		Target:             "worker1",
	})
	resp2 := d.handleQueueWrite(req2)
	if resp2.Success {
		t.Fatal("expected backpressure error")
	}
	if resp2.Error.Code != uds.ErrCodeBackpressure {
		t.Errorf("error code = %q, want %q", resp2.Error.Code, uds.ErrCodeBackpressure)
	}
}

func TestQueueWriteTask_ValidationErrors(t *testing.T) {
	d := newTestDaemon(t)

	tests := []struct {
		name   string
		params QueueWriteParams
	}{
		{"missing command_id", QueueWriteParams{Type: "task", Content: "c", Purpose: "p", AcceptanceCriteria: "ac", BloomLevel: 3, Target: "worker1"}},
		{"missing content", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Purpose: "p", AcceptanceCriteria: "ac", BloomLevel: 3, Target: "worker1"}},
		{"missing purpose", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Content: "c", AcceptanceCriteria: "ac", BloomLevel: 3, Target: "worker1"}},
		{"missing acceptance_criteria", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Content: "c", Purpose: "p", BloomLevel: 3, Target: "worker1"}},
		{"bloom_level 0", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Content: "c", Purpose: "p", AcceptanceCriteria: "ac", BloomLevel: 0, Target: "worker1"}},
		{"bloom_level 7", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Content: "c", Purpose: "p", AcceptanceCriteria: "ac", BloomLevel: 7, Target: "worker1"}},
		{"missing target", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Content: "c", Purpose: "p", AcceptanceCriteria: "ac", BloomLevel: 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeQueueWriteRequest(t, tt.params)
			resp := d.handleQueueWrite(req)
			if resp.Success {
				t.Fatal("expected validation error")
			}
			if resp.Error.Code != uds.ErrCodeValidation {
				t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
			}
		})
	}
}

func TestQueueWriteNotification_Basic(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:             "notification",
		CommandID:        "cmd_0000000001_abcdef01",
		Content:          "task completed successfully",
		SourceResultID:   "res_0000000001_abcdef01",
		NotificationType: "command_completed",
	})

	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["id"] == "" {
		t.Error("expected non-empty id")
	}

	// Verify file
	path := filepath.Join(d.maestroDir, "queue", "orchestrator.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var nq model.NotificationQueue
	if err := yamlv3.Unmarshal(data, &nq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if len(nq.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(nq.Notifications))
	}
	if nq.Notifications[0].SourceResultID != "res_0000000001_abcdef01" {
		t.Errorf("source_result_id = %q, want %q", nq.Notifications[0].SourceResultID, "res_0000000001_abcdef01")
	}
	if nq.Notifications[0].Type != "command_completed" {
		t.Errorf("type = %q, want %q", nq.Notifications[0].Type, "command_completed")
	}
}

func TestQueueWriteNotification_Idempotency(t *testing.T) {
	d := newTestDaemon(t)

	params := QueueWriteParams{
		Type:             "notification",
		CommandID:        "cmd_0000000001_abcdef01",
		Content:          "task completed",
		SourceResultID:   "res_0000000001_abcdef01",
		NotificationType: "command_completed",
	}

	req1 := makeQueueWriteRequest(t, params)
	resp1 := d.handleQueueWrite(req1)
	if !resp1.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp1.Error)
	}

	var result1 map[string]string
	json.Unmarshal(resp1.Data, &result1)
	firstID := result1["id"]

	// Same source_result_id should return duplicate
	req2 := makeQueueWriteRequest(t, params)
	resp2 := d.handleQueueWrite(req2)
	if !resp2.Success {
		t.Fatalf("write 2: expected success (duplicate), got error: %v", resp2.Error)
	}

	var result2 map[string]string
	json.Unmarshal(resp2.Data, &result2)

	if result2["id"] != firstID {
		t.Errorf("duplicate should return same id: got %q, want %q", result2["id"], firstID)
	}
	if result2["duplicate"] != "true" {
		t.Errorf("duplicate flag = %q, want %q", result2["duplicate"], "true")
	}

	// Verify only 1 notification in file
	path := filepath.Join(d.maestroDir, "queue", "orchestrator.yaml")
	data, _ := os.ReadFile(path)
	var nq model.NotificationQueue
	yamlv3.Unmarshal(data, &nq)
	if len(nq.Notifications) != 1 {
		t.Errorf("expected 1 notification, got %d", len(nq.Notifications))
	}
}

func TestQueueWriteNotification_ValidationErrors(t *testing.T) {
	d := newTestDaemon(t)

	tests := []struct {
		name   string
		params QueueWriteParams
	}{
		{"missing command_id", QueueWriteParams{Type: "notification", Content: "c", SourceResultID: "res_0000000001_abcdef01", NotificationType: "command_completed"}},
		{"missing content", QueueWriteParams{Type: "notification", CommandID: "cmd_0000000001_abcdef01", SourceResultID: "res_0000000001_abcdef01", NotificationType: "command_completed"}},
		{"missing source_result_id", QueueWriteParams{Type: "notification", CommandID: "cmd_0000000001_abcdef01", Content: "c", NotificationType: "command_completed"}},
		{"missing notification_type", QueueWriteParams{Type: "notification", CommandID: "cmd_0000000001_abcdef01", Content: "c", SourceResultID: "res_0000000001_abcdef01"}},
		{"invalid notification_type", QueueWriteParams{Type: "notification", CommandID: "cmd_0000000001_abcdef01", Content: "c", SourceResultID: "res_0000000001_abcdef01", NotificationType: "invalid_type"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeQueueWriteRequest(t, tt.params)
			resp := d.handleQueueWrite(req)
			if resp.Success {
				t.Fatal("expected validation error")
			}
			if resp.Error.Code != uds.ErrCodeValidation {
				t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
			}
		})
	}
}

func TestQueueWrite_InvalidType(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "invalid",
		Content: "test",
	})
	resp := d.handleQueueWrite(req)
	if resp.Success {
		t.Fatal("expected validation error for invalid type")
	}
}

func TestQueueWriteTask_WithBlockedBy(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:               "task",
		CommandID:          "cmd_0000000001_abcdef01",
		Content:            "task with deps",
		Purpose:            "test",
		AcceptanceCriteria: "criteria",
		BloomLevel:         4,
		Target:             "worker1",
		BlockedBy:          []string{"task_0000000001_11111111", "task_0000000001_22222222"},
	})

	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify blocked_by persisted
	path := filepath.Join(d.maestroDir, "queue", "worker1.yaml")
	data, _ := os.ReadFile(path)
	var tq model.TaskQueue
	yamlv3.Unmarshal(data, &tq)
	if len(tq.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tq.Tasks))
	}
	if len(tq.Tasks[0].BlockedBy) != 2 {
		t.Errorf("blocked_by length = %d, want 2", len(tq.Tasks[0].BlockedBy))
	}
}

func TestQueueWriteTask_BlockedByValidation(t *testing.T) {
	d := newTestDaemon(t)

	tests := []struct {
		name      string
		blockedBy []string
	}{
		{"invalid ID format", []string{"invalid-id"}},
		{"non-task ID", []string{"cmd_0000000001_abcdef01"}},
		{"duplicate IDs", []string{"task_0000000001_abcdef01", "task_0000000001_abcdef01"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeQueueWriteRequest(t, QueueWriteParams{
				Type:               "task",
				CommandID:          "cmd_0000000001_abcdef01",
				Content:            "task",
				Purpose:            "purpose",
				AcceptanceCriteria: "criteria",
				BloomLevel:         3,
				Target:             "worker1",
				BlockedBy:          tt.blockedBy,
			})
			resp := d.handleQueueWrite(req)
			if resp.Success {
				t.Fatal("expected validation error for blocked_by")
			}
			if resp.Error.Code != uds.ErrCodeValidation {
				t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
			}
		})
	}
}

func TestQueueWriteCancelRequest_Unsubmitted(t *testing.T) {
	d := newTestDaemon(t)

	// First create a command in planner queue
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "test command",
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("create command: %v", resp.Error)
	}

	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	commandID := result["id"]

	// Cancel it (unsubmitted — no state file)
	cancelReq := makeQueueWriteRequest(t, QueueWriteParams{
		Type:      "cancel-request",
		CommandID: commandID,
		Reason:    "user requested",
	})
	cancelResp := d.handleQueueWrite(cancelReq)
	if !cancelResp.Success {
		t.Fatalf("cancel: expected success, got error: %v", cancelResp.Error)
	}

	var cancelResult map[string]string
	json.Unmarshal(cancelResp.Data, &cancelResult)
	if cancelResult["status"] != "cancelled" {
		t.Errorf("cancel status = %q, want %q", cancelResult["status"], "cancelled")
	}

	// Verify command is cancelled in queue
	path := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	data, _ := os.ReadFile(path)
	var cq model.CommandQueue
	yamlv3.Unmarshal(data, &cq)
	if cq.Commands[0].Status != model.StatusCancelled {
		t.Errorf("queue command status = %q, want %q", cq.Commands[0].Status, model.StatusCancelled)
	}
}

func TestQueueWriteCancelRequest_Submitted(t *testing.T) {
	d := newTestDaemon(t)

	commandID := "cmd_0000000001_abcdef01"
	// Create state file (simulating submitted command)
	setupCommandState(t, d, commandID, []string{"task_0000000001_abcdef01"})

	// Also create queue entry
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{ID: commandID, Content: "test", Status: model.StatusInProgress, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Cancel request
	cancelReq := makeQueueWriteRequest(t, QueueWriteParams{
		Type:      "cancel-request",
		CommandID: commandID,
		Reason:    "timeout",
	})
	cancelResp := d.handleQueueWrite(cancelReq)
	if !cancelResp.Success {
		t.Fatalf("cancel: expected success, got error: %v", cancelResp.Error)
	}

	var result map[string]string
	json.Unmarshal(cancelResp.Data, &result)
	if result["status"] != "cancel_requested" {
		t.Errorf("cancel status = %q, want %q", result["status"], "cancel_requested")
	}

	// Verify state updated
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, _ := os.ReadFile(statePath)
	var state model.CommandState
	yamlv3.Unmarshal(sdata, &state)
	if !state.Cancel.Requested {
		t.Error("expected cancel.requested to be true")
	}
}

func TestQueueWriteCancelRequest_Idempotent(t *testing.T) {
	d := newTestDaemon(t)

	commandID := "cmd_0000000001_abcdef01"
	setupCommandState(t, d, commandID, []string{"task_0000000001_abcdef01"})

	cancelReq := makeQueueWriteRequest(t, QueueWriteParams{
		Type:      "cancel-request",
		CommandID: commandID,
		Reason:    "timeout",
	})

	// First cancel
	resp1 := d.handleQueueWrite(cancelReq)
	if !resp1.Success {
		t.Fatalf("cancel 1: expected success, got error: %v", resp1.Error)
	}

	// Second cancel (idempotent)
	resp2 := d.handleQueueWrite(cancelReq)
	if !resp2.Success {
		t.Fatalf("cancel 2: expected success (idempotent), got error: %v", resp2.Error)
	}

	var result map[string]string
	json.Unmarshal(resp2.Data, &result)
	if result["status"] != "already_requested" {
		t.Errorf("cancel status = %q, want %q", result["status"], "already_requested")
	}
}

func TestQueueWriteCommand_DefaultPriority(t *testing.T) {
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "test",
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	path := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	data, _ := os.ReadFile(path)
	var cq model.CommandQueue
	yamlv3.Unmarshal(data, &cq)
	if cq.Commands[0].Priority != 100 {
		t.Errorf("default priority = %d, want 100", cq.Commands[0].Priority)
	}
}

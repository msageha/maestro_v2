package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func TestQueueWriteMessage_Basic(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "message",
		Target:  "orchestrator",
		Content: "続きのタスクを進めてください",
	})

	resp := d.api.handleQueueWrite(req)
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
	ntf := nq.Notifications[0]
	if ntf.Type != model.NotificationTypeUserMessage {
		t.Errorf("type = %q, want %q", ntf.Type, model.NotificationTypeUserMessage)
	}
	if ntf.Content != "続きのタスクを進めてください" {
		t.Errorf("content = %q, want %q", ntf.Content, "続きのタスクを進めてください")
	}
	if ntf.Status != model.StatusPending {
		t.Errorf("status = %q, want %q", ntf.Status, model.StatusPending)
	}
	if ntf.SourceResultID != "user:"+ntf.ID {
		t.Errorf("source_result_id = %q, want %q", ntf.SourceResultID, "user:"+ntf.ID)
	}
	if ntf.Priority != model.DefaultPriority {
		t.Errorf("priority = %d, want %d (default)", ntf.Priority, model.DefaultPriority)
	}
}

func TestQueueWriteMessage_DistinctEntriesForSameContent(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	params := QueueWriteParams{Type: "message", Target: "orchestrator", Content: "再送テスト"}
	for i := 0; i < 2; i++ {
		resp := d.api.handleQueueWrite(makeQueueWriteRequest(t, params))
		if !resp.Success {
			t.Fatalf("write %d: expected success, got error: %v", i+1, resp.Error)
		}
	}

	path := filepath.Join(d.maestroDir, "queue", "orchestrator.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var nq model.NotificationQueue
	if err := yamlv3.Unmarshal(data, &nq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	// Unlike result-backed notifications, user messages are never deduped:
	// the same text sent twice is two deliveries.
	if len(nq.Notifications) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(nq.Notifications))
	}
	if nq.Notifications[0].ID == nq.Notifications[1].ID {
		t.Error("expected distinct notification IDs")
	}
}

func TestQueueWriteMessage_ValidationErrors(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	tests := []struct {
		name    string
		params  QueueWriteParams
		wantMsg string
	}{
		{"missing content", QueueWriteParams{Type: "message", Target: "orchestrator"}, "content is required"},
		{"planner target", QueueWriteParams{Type: "message", Target: "planner", Content: "c"}, "target=orchestrator"},
		{"worker target", QueueWriteParams{Type: "message", Target: "worker1", Content: "c"}, "target=orchestrator"},
		{"empty target", QueueWriteParams{Type: "message", Content: "c"}, "target=orchestrator"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp := d.api.handleQueueWrite(makeQueueWriteRequest(t, tt.params))
			if resp.Success {
				t.Fatal("expected validation error")
			}
			if resp.Error.Code != uds.ErrCodeValidation {
				t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
			}
			if !strings.Contains(resp.Error.Message, tt.wantMsg) {
				t.Errorf("error message = %q, want substring %q", resp.Error.Message, tt.wantMsg)
			}
		})
	}
}

func TestQueueWriteMessage_ContentSizeLimit(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Limits.MaxEntryContentBytes = 10

	resp := d.api.handleQueueWrite(makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "message",
		Target:  "orchestrator",
		Content: "this content exceeds the ten byte limit",
	}))
	if resp.Success {
		t.Fatal("expected content size validation error")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestQueueWriteMessage_RoleCLIOnly(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	for _, role := range []string{uds.RoleOrchestrator, uds.RolePlanner, uds.RoleWorker} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			req := makeQueueWriteRequest(t, QueueWriteParams{
				Type:    "message",
				Target:  "orchestrator",
				Content: "should be rejected",
			})
			req.CallerRole = role
			resp := d.api.handleQueueWrite(req)
			if resp.Success {
				t.Fatalf("expected role rejection for %s", role)
			}
		})
	}
}

// TestQueueWriteMessage_BypassesContinuousGate pins the deliberate difference
// from type=command: a paused continuous run must not block the user from
// reaching the Orchestrator (that is often exactly when they need to).
func TestQueueWriteMessage_BypassesContinuousGate(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Continuous.Enabled = true
	writeGateContinuousState(t, d.maestroDir, model.ContinuousStatusPaused, "max_consecutive_failures")

	resp := d.api.handleQueueWrite(makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "message",
		Target:  "orchestrator",
		Content: "pause の原因を報告して",
	}))
	if !resp.Success {
		t.Fatalf("expected success while continuous is paused, got error: %+v", resp.Error)
	}
}

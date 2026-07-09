package dispatch

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// captureExecutor records the requests it receives and succeeds.
type captureExecutor struct {
	requests []agent.ExecRequest
}

func (c *captureExecutor) Execute(req agent.ExecRequest) agent.ExecResult {
	c.requests = append(c.requests, req)
	return agent.ExecResult{Success: true}
}
func (c *captureExecutor) Close() error                                  { return nil }
func (c *captureExecutor) RespawnPaneToProjectRoot(string, string) error { return nil }

func TestDispatchNotification_UserMessageCarriesContent(t *testing.T) {
	t.Parallel()
	exec := &captureExecutor{}
	d := newRetryTestDispatcher(0, 0, exec)

	err := d.DispatchNotification(&model.Notification{
		ID:      "ntf_001",
		Type:    model.NotificationTypeUserMessage,
		Content: "優先度を見直してから続行して",
		Status:  model.StatusPending,
	})
	if err != nil {
		t.Fatalf("DispatchNotification: %v", err)
	}
	if len(exec.requests) != 1 {
		t.Fatalf("expected 1 exec request, got %d", len(exec.requests))
	}
	req := exec.requests[0]
	if req.AgentID != "orchestrator" {
		t.Errorf("agent = %q, want orchestrator", req.AgentID)
	}
	if !strings.HasPrefix(req.Message, "[maestro] kind:user_message\n") {
		t.Errorf("message missing user_message header: %q", req.Message)
	}
	if !strings.Contains(req.Message, "優先度を見直してから続行して") {
		t.Errorf("message missing content: %q", req.Message)
	}
	if strings.Contains(req.Message, "results/planner.yaml") {
		t.Errorf("user message must not point at a result file: %q", req.Message)
	}
}

func TestDispatchNotification_CommandCompletedKeepsResultPointer(t *testing.T) {
	t.Parallel()
	exec := &captureExecutor{}
	d := newRetryTestDispatcher(0, 0, exec)

	err := d.DispatchNotification(&model.Notification{
		ID:        "ntf_002",
		CommandID: "cmd_001",
		Type:      model.NotificationTypeCommandCompleted,
		Content:   "ignored by envelope",
		Status:    model.StatusPending,
	})
	if err != nil {
		t.Fatalf("DispatchNotification: %v", err)
	}
	if len(exec.requests) != 1 {
		t.Fatalf("expected 1 exec request, got %d", len(exec.requests))
	}
	req := exec.requests[0]
	if !strings.Contains(req.Message, "kind:command_completed") {
		t.Errorf("message missing kind header: %q", req.Message)
	}
	if !strings.Contains(req.Message, "results/planner.yaml") {
		t.Errorf("result-backed notification must keep the result pointer: %q", req.Message)
	}
}

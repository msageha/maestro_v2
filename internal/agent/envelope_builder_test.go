package agent

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestBuildWorkerEnvelope(t *testing.T) {
	task := model.Task{
		ID:                 "task_1771722060_b7c1d4e9",
		CommandID:          "cmd_1771722000_a3f2b7c1",
		Purpose:            "Implement login endpoint",
		Content:            "Create POST /api/login with JWT auth",
		AcceptanceCriteria: "Tests pass, endpoint returns 200",
		Constraints:        []string{"Use JWT", "No third-party auth"},
		ToolsHint:          []string{"context7", "grep"},
	}

	envelope := BuildWorkerEnvelope(task, "worker1", 3, 1)

	// Verify header
	if !strings.Contains(envelope, "[maestro] task_id:task_1771722060_b7c1d4e9 command_id:cmd_1771722000_a3f2b7c1 lease_epoch:3 attempt:1") {
		t.Error("missing or incorrect header")
	}

	// Verify agent_id field
	if !strings.Contains(envelope, "agent_id: worker1") {
		t.Error("missing agent_id field")
	}

	// Verify key-value format fields (spec §5.8.1)
	if !strings.Contains(envelope, "purpose: Implement login endpoint") {
		t.Error("missing purpose field")
	}
	if !strings.Contains(envelope, "content: Create POST /api/login with JWT auth") {
		t.Error("missing content field")
	}
	if !strings.Contains(envelope, "acceptance_criteria: Tests pass, endpoint returns 200") {
		t.Error("missing acceptance_criteria field")
	}
	if !strings.Contains(envelope, "constraints: Use JWT, No third-party auth") {
		t.Error("missing constraints field (comma-separated)")
	}
	if !strings.Contains(envelope, "tools_hint: context7, grep") {
		t.Error("missing tools_hint field (comma-separated)")
	}

	// Verify result template with Japanese labels (spec format)
	if !strings.Contains(envelope, "完了時: maestro result write worker1 --task-id task_1771722060_b7c1d4e9 --command-id cmd_1771722000_a3f2b7c1 --lease-epoch 3") {
		t.Error("incorrect result template")
	}
	if !strings.Contains(envelope, "失敗時に部分変更あり: 上記に加えて --partial-changes --no-retry-safe") {
		t.Error("missing partial changes guidance")
	}
}

func TestBuildWorkerEnvelope_EmptyOptionals(t *testing.T) {
	task := model.Task{
		ID:                 "task_1771722060_b7c1d4e9",
		CommandID:          "cmd_1771722000_a3f2b7c1",
		Purpose:            "Simple task",
		Content:            "Do something",
		AcceptanceCriteria: "Done",
		Constraints:        nil,
		ToolsHint:          nil,
	}

	envelope := BuildWorkerEnvelope(task, "worker2", 1, 1)

	// Empty constraints/tools_hint should show "なし" per spec
	if !strings.Contains(envelope, "constraints: なし") {
		t.Error("missing constraints default 'なし'")
	}
	if !strings.Contains(envelope, "tools_hint: なし") {
		t.Error("missing tools_hint default 'なし'")
	}
}

func TestBuildPlannerEnvelope(t *testing.T) {
	cmd := model.Command{
		ID:      "cmd_1771722000_a3f2b7c1",
		Content: "Implement user authentication system",
	}

	envelope := BuildPlannerEnvelope(cmd, 2, 1)

	if !strings.Contains(envelope, "[maestro] command_id:cmd_1771722000_a3f2b7c1 lease_epoch:2 attempt:1") {
		t.Error("missing or incorrect header")
	}
	if !strings.Contains(envelope, "content: Implement user authentication system") {
		t.Error("missing content field")
	}
	if !strings.Contains(envelope, "タスク分解後: maestro plan submit --command-id cmd_1771722000_a3f2b7c1 --tasks-file plan.yaml") {
		t.Error("missing plan submit template")
	}
	if !strings.Contains(envelope, "全タスク完了後: maestro plan complete --command-id cmd_1771722000_a3f2b7c1 --summary") {
		t.Error("missing plan complete template")
	}
}

func TestBuildOrchestratorNotificationEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		cmdID    string
		ntfType  model.NotificationType
		expected string
	}{
		{
			"completed",
			"cmd_1771722000_a3f2b7c1",
			"command_completed",
			"[maestro] kind:command_completed command_id:cmd_1771722000_a3f2b7c1 status:completed\nresults/planner.yaml を確認してください",
		},
		{
			"failed",
			"cmd_1771722000_a3f2b7c1",
			"command_failed",
			"[maestro] kind:command_completed command_id:cmd_1771722000_a3f2b7c1 status:failed\nresults/planner.yaml を確認してください",
		},
		{
			"cancelled",
			"cmd_1771722000_a3f2b7c1",
			"command_cancelled",
			"[maestro] kind:command_completed command_id:cmd_1771722000_a3f2b7c1 status:cancelled\nresults/planner.yaml を確認してください",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildOrchestratorNotificationEnvelope(tt.cmdID, tt.ntfType)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestBuildTaskResultNotification(t *testing.T) {
	got := BuildTaskResultNotification(
		"cmd_1771722000_a3f2b7c1",
		"task_1771722060_b7c1d4e9",
		"worker3",
		"completed",
	)

	if !strings.Contains(got, "[maestro] kind:task_result") {
		t.Error("missing kind header")
	}
	if !strings.Contains(got, "command_id:cmd_1771722000_a3f2b7c1") {
		t.Error("missing command_id")
	}
	if !strings.Contains(got, "task_id:task_1771722060_b7c1d4e9") {
		t.Error("missing task_id")
	}
	if !strings.Contains(got, "worker_id:worker3") {
		t.Error("missing worker_id")
	}
	if !strings.Contains(got, "status:completed") {
		t.Error("missing status")
	}
	if !strings.Contains(got, "results/worker3.yaml") {
		t.Error("missing results file reference")
	}
}

package agent

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// --- Envelope Builders ---

// BuildWorkerEnvelope creates the delivery envelope for a Worker task.
// Format matches spec §5.8.1 Worker 向けタスク配信エンベロープ.
func BuildWorkerEnvelope(task model.Task, workerID string, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] task_id:%s command_id:%s lease_epoch:%d attempt:%d\n",
		task.ID, task.CommandID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "purpose: %s\n", task.Purpose)
	fmt.Fprintf(&sb, "content: %s\n", task.Content)
	fmt.Fprintf(&sb, "acceptance_criteria: %s\n", task.AcceptanceCriteria)
	constraintsStr := "なし"
	if len(task.Constraints) > 0 {
		constraintsStr = strings.Join(task.Constraints, ", ")
	}
	fmt.Fprintf(&sb, "constraints: %s\n", constraintsStr)
	toolsHintStr := "なし"
	if len(task.ToolsHint) > 0 {
		toolsHintStr = strings.Join(task.ToolsHint, ", ")
	}
	fmt.Fprintf(&sb, "tools_hint: %s\n", toolsHintStr)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "完了時: maestro result write %s --task-id %s --command-id %s --lease-epoch %d --status <completed|failed> --summary \"...\"\n",
		workerID, task.ID, task.CommandID, leaseEpoch)
	sb.WriteString("失敗時に部分変更あり: 上記に加えて --partial-changes --no-retry-safe")
	return sb.String()
}

// BuildPlannerEnvelope creates the delivery envelope for a Planner command.
// Format matches spec §5.8.1 Planner 向けコマンド配信エンベロープ.
func BuildPlannerEnvelope(cmd model.Command, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] command_id:%s lease_epoch:%d attempt:%d\n",
		cmd.ID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "content: %s\n", cmd.Content)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "タスク分解後: maestro plan submit --command-id %s --tasks-file plan.yaml\n", cmd.ID)
	fmt.Fprintf(&sb, "全タスク完了後: maestro plan complete --command-id %s --summary \"...\"", cmd.ID)
	return sb.String()
}

// BuildOrchestratorNotificationEnvelope creates the envelope for an Orchestrator notification.
// Format matches spec §5.8.1 Orchestrator 向け通知配信エンベロープ.
func BuildOrchestratorNotificationEnvelope(commandID string, notificationType model.NotificationType) string {
	terminalStatus := mapNotificationTypeToStatus(notificationType)
	return fmt.Sprintf("[maestro] kind:command_completed command_id:%s status:%s\nresults/planner.yaml を確認してください",
		commandID, terminalStatus)
}

func mapNotificationTypeToStatus(nt model.NotificationType) string {
	switch nt {
	case "command_completed":
		return "completed"
	case "command_failed":
		return "failed"
	case "command_cancelled":
		return "cancelled"
	default:
		return string(nt)
	}
}

// BuildTaskResultNotification creates a side-channel notification for the Planner.
func BuildTaskResultNotification(commandID, taskID, workerID, taskStatus string) string {
	return fmt.Sprintf("[maestro] kind:task_result command_id:%s task_id:%s worker_id:%s status:%s\nresults/%s.yaml を確認してください",
		commandID, taskID, workerID, taskStatus, workerID)
}

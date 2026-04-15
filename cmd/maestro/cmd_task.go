package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runTask dispatches task subcommands (currently: heartbeat).
func (a *cliApp) runTask(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro task: missing subcommand\nusage: maestro task <heartbeat> [options]"}
	}
	switch args[0] {
	case "heartbeat":
		return a.runTaskHeartbeat(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro task: unknown subcommand: %s\nusage: maestro task heartbeat [options]", args[0])}
	}
}

// runTaskHeartbeat sends a heartbeat for an active task via UDS.
func (a *cliApp) runTaskHeartbeat(args []string) error {
	cmd := NewCommand("maestro task heartbeat", "maestro task heartbeat --task-id <id> --worker-id <id> --epoch <n>")
	var taskID, workerID string
	var epoch int
	cmd.RequiredString(&taskID, "task-id", "Task ID to send heartbeat for")
	cmd.RequiredString(&workerID, "worker-id", "Worker ID that owns the task")
	cmd.RequiredInt(&epoch, "epoch", -1, "Lease epoch number for fencing")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(taskID); err != nil {
		return cmd.Errorf("invalid --task-id: %v", err)
	}
	if err := validate.ID(workerID); err != nil {
		return cmd.Errorf("invalid --worker-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("task heartbeat")
	if err != nil {
		return err
	}

	params := map[string]interface{}{
		"task_id":   taskID,
		"worker_id": workerID,
		"epoch":     epoch,
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("task_heartbeat", params)
	if err != nil {
		return fmt.Errorf("maestro task heartbeat: %w", err)
	}

	if !resp.Success {
		if resp.Error != nil {
			// Special handling for max runtime exceeded - exit silently with error code
			if resp.Error.Code == uds.ErrCodeMaxRuntimeExceeded {
				return &CLIError{Code: 2, Silent: true}
			}
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro task heartbeat: [%s] %s", resp.Error.Code, resp.Error.Message)}
		}
		return &CLIError{Code: 1, Msg: "maestro task heartbeat: heartbeat failed"}
	}

	// Success - no output (for use in scripts)
	return nil
}

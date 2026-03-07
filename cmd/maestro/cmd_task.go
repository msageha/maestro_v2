package main

import (
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/uds"
)

// runTask dispatches task subcommands (currently: heartbeat).
func runTask(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro task: missing subcommand\nusage: maestro task <heartbeat> [options]"}
	}
	switch args[0] {
	case "heartbeat":
		return runTaskHeartbeat(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro task: unknown subcommand: %s\nusage: maestro task heartbeat [options]", args[0])}
	}
}

// runTaskHeartbeat sends a heartbeat for an active task via UDS.
func runTaskHeartbeat(args []string) error {
	fs := newFlagSet("maestro task heartbeat")
	var taskID, workerID string
	var epoch int
	fs.StringVar(&taskID, "task-id", "", "")
	fs.StringVar(&workerID, "worker-id", "", "")
	fs.IntVar(&epoch, "epoch", 0, "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro task heartbeat: %v\nusage: maestro task heartbeat --task-id <id> --worker-id <id> --epoch <n>", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro task heartbeat: unexpected argument: %s\nusage: maestro task heartbeat --task-id <id> --worker-id <id> --epoch <n>", fs.Arg(0))}
	}

	if taskID == "" {
		return &CLIError{Code: 1, Msg: "maestro task heartbeat: --task-id is required"}
	}
	if workerID == "" {
		return &CLIError{Code: 1, Msg: "maestro task heartbeat: --worker-id is required"}
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

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
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

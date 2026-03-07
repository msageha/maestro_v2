package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runQueue dispatches queue subcommands (currently: write).
func runQueue(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro queue: missing subcommand\nusage: maestro queue <write> [options]"}
	}
	switch args[0] {
	case "write":
		return runQueueWrite(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue: unknown subcommand: %s\nusage: maestro queue write <target> [options]", args[0])}
	}
}

// runQueueWrite enqueues a command, task, notification, or cancel-request via UDS.
func runQueueWrite(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro queue write: missing target\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]"}
	}

	target := args[0]

	fs := newFlagSet("maestro queue write")
	var writeType, content, commandID, purpose, acceptanceCriteria, sourceResultID, notificationType, reason string
	var bloomLevel, priority int
	var blockedBy, constraints, toolsHint stringSliceFlag

	fs.StringVar(&writeType, "type", "", "")
	fs.StringVar(&content, "content", "", "")
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&purpose, "purpose", "", "")
	fs.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "")
	fs.IntVar(&bloomLevel, "bloom-level", 0, "")
	fs.IntVar(&priority, "priority", 0, "")
	fs.StringVar(&sourceResultID, "source-result-id", "", "")
	fs.StringVar(&notificationType, "notification-type", "", "")
	fs.Var(&blockedBy, "blocked-by", "")
	fs.Var(&constraints, "constraint", "")
	fs.Var(&toolsHint, "tools-hint", "")
	var skillRefs stringSliceFlag
	fs.Var(&skillRefs, "skill-refs", "")
	fs.StringVar(&reason, "reason", "", "")

	if err := fs.Parse(args[1:]); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: %v\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: unexpected argument: %s\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]", fs.Arg(0))}
	}

	if writeType == "" {
		return &CLIError{Code: 1, Msg: "maestro queue write: --type is required\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]"}
	}

	params := map[string]any{
		"target": target,
		"type":   writeType,
	}

	switch writeType {
	case "command":
		if content == "" {
			return &CLIError{Code: 1, Msg: "maestro queue write: --content is required for type=command"}
		}
		params["content"] = content
		if priority > 0 {
			params["priority"] = priority
		}
	case "task":
		if commandID == "" || content == "" || purpose == "" || acceptanceCriteria == "" || bloomLevel == 0 {
			return &CLIError{Code: 1, Msg: "maestro queue write: required for type=task: --command-id, --content, --purpose, --acceptance-criteria, --bloom-level"}
		}
		if err := validate.ValidateID(commandID); err != nil {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: invalid --command-id: %v", err)}
		}
		if bloomLevel < 1 || bloomLevel > 6 {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: --bloom-level must be between 1 and 6, got %d", bloomLevel)}
		}
		params["command_id"] = commandID
		params["content"] = content
		params["purpose"] = purpose
		params["acceptance_criteria"] = acceptanceCriteria
		params["bloom_level"] = bloomLevel
		if priority > 0 {
			params["priority"] = priority
		}
		if len(blockedBy) > 0 {
			params["blocked_by"] = blockedBy
		}
		if len(constraints) > 0 {
			params["constraints"] = constraints
		}
		if len(toolsHint) > 0 {
			params["tools_hint"] = toolsHint
		}
		if len(skillRefs) > 0 {
			params["skill_refs"] = skillRefs
		}
	case "notification":
		if commandID == "" || content == "" || sourceResultID == "" {
			return &CLIError{Code: 1, Msg: "maestro queue write: required for type=notification: --command-id, --content, --source-result-id"}
		}
		if err := validate.ValidateID(commandID); err != nil {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: invalid --command-id: %v", err)}
		}
		params["command_id"] = commandID
		params["content"] = content
		params["source_result_id"] = sourceResultID
		if notificationType != "" {
			params["notification_type"] = notificationType
		}
		if priority > 0 {
			params["priority"] = priority
		}
	case "cancel-request":
		if commandID == "" {
			return &CLIError{Code: 1, Msg: "maestro queue write: required for type=cancel-request: --command-id"}
		}
		if err := validate.ValidateID(commandID); err != nil {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: invalid --command-id: %v", err)}
		}
		params["command_id"] = commandID
		if reason != "" {
			params["reason"] = reason
		}
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: unknown type: %s\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]", writeType)}
	}

	return sendQueueWrite(params)
}

// sendQueueWrite sends a queue_write request to the daemon via UDS.
func sendQueueWrite(params map[string]any) error {
	maestroDir, err := requireMaestroDir("queue write")
	if err != nil {
		return err
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		return fmt.Errorf("maestro queue write: %w", err)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		if code == "BACKPRESSURE" {
			return &CLIError{Code: 2, Msg: fmt.Sprintf("maestro queue write: [%s] %s", code, msg)}
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: [%s] %s", code, msg)}
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err == nil {
		if id, ok := result["id"]; ok {
			fmt.Println(id)
			return nil
		}
		if cid, ok := result["command_id"]; ok {
			fmt.Println(cid)
			return nil
		}
	}
	out, err := json.MarshalIndent(resp.Data, "", "  ")
	if err != nil {
		return fmt.Errorf("maestro queue write: format response json: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

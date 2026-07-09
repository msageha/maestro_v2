package main

import (
	"fmt"
	"io"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runVerify dispatches verify subcommands.
func (a *cliApp) runVerify(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro verify: missing subcommand\nusage: maestro verify write --command-id <id> [--config-file <path>|-]"}
	}
	switch args[0] {
	case "write":
		return a.runVerifyWrite(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro verify: unknown subcommand: %s\nusage: maestro verify write --command-id <id> [--config-file <path>|-]", args[0])}
	}
}

func (a *cliApp) runVerifyWrite(args []string) error {
	cmd := NewCommand("maestro verify write", "maestro verify write --command-id <id> [--config-file <path>|-]")
	configFile := "-"
	commandID := ""
	cmd.StringVar(&configFile, "config-file", "-", "Path to verify YAML file (default: stdin)")
	cmd.RequiredString(&commandID, "command-id", "Command ID this verify config applies to")
	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("verify write")
	if err != nil {
		return err
	}

	data, err := readVerifyConfigInput(configFile)
	if err != nil {
		return err
	}
	cfg, err := model.ParseVerifyConfigYAML(data)
	if err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro verify write: %v", err)}
	}
	if cfg.IsEmpty() {
		return &CLIError{Code: 1, Msg: "maestro verify write: verify config must contain at least one command"}
	}
	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("verify_write", map[string]any{
		"config_data": string(data),
		"command_id":  commandID,
	})
	if err != nil {
		return fmt.Errorf("maestro verify write: %w", err)
	}
	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		if code == uds.ErrCodeValidation {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro verify write: %s", msg)}
		}
		return udsCLIError("maestro verify write", resp)
	}
	fmt.Println("verify config written")
	return nil
}

// readVerifyConfigInput reads the verify config from stdin or a file. Both
// sources are capped at maxInlineUDSPayloadBytes because the content is
// sent inline (config_data) in a single UDS frame either way.
func readVerifyConfigInput(configFile string) ([]byte, error) {
	if configFile == "" || configFile == "-" || configFile == "/dev/stdin" {
		if isStdinTerminal() {
			return nil, &CLIError{Code: 1, Msg: "maestro verify write: stdin is a terminal and no config input was piped; pass --config-file <path> or pipe the verify YAML into stdin"}
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(maxInlineUDSPayloadBytes)+1))
		if err == nil && len(data) > maxInlineUDSPayloadBytes {
			return nil, fmt.Errorf("maestro verify write: stdin input exceeds the %d-byte inline limit (UDS frame cap)", maxInlineUDSPayloadBytes)
		}
		return data, err
	}
	cleaned, err := validate.FilePath(configFile)
	if err != nil {
		return nil, fmt.Errorf("maestro verify write: invalid --config-file: %w", err)
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		return nil, fmt.Errorf("maestro verify write: stat config file: %w", err)
	}
	if info.Size() > int64(maxInlineUDSPayloadBytes) {
		return nil, fmt.Errorf("maestro verify write: config file exceeds the %d-byte inline limit (UDS frame cap) (got %d)", maxInlineUDSPayloadBytes, info.Size())
	}
	data, err := os.ReadFile(cleaned) //nolint:gosec // configFile is validated above and intentionally user-specified
	if err != nil {
		return nil, fmt.Errorf("maestro verify write: read config file: %w", err)
	}
	return data, nil
}

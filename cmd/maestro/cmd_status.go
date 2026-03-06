package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/status"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runStatus displays the current formation status.
func runStatus(args []string) error {
	fs := newFlagSet("maestro status")
	var jsonOutput bool
	fs.BoolVar(&jsonOutput, "json", false, "")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro status: %v\nusage: maestro status [--json]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro status: unexpected argument: %s\nusage: maestro status [--json]", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("status")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro status: load config: %w", err)
	}
	// HIGH-16: Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro status: invalid project name: %w", err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	if err := status.Run(maestroDir, jsonOutput); err != nil {
		return fmt.Errorf("maestro status: %w", err)
	}
	return nil
}

// runDashboard regenerates the dashboard.md file.
func runDashboard(args []string) error {
	fs := newFlagSet("maestro dashboard")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro dashboard: %v\nusage: maestro dashboard", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro dashboard: unexpected argument: %s\nusage: maestro dashboard", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("dashboard")
	if err != nil {
		return err
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("dashboard", nil)
	if err != nil {
		return fmt.Errorf("maestro dashboard: %w", err)
	}

	if !resp.Success {
		msg := "unknown error"
		if resp.Error != nil {
			msg = resp.Error.Message
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro dashboard: %s", msg)}
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return fmt.Errorf("maestro dashboard: parse response: %w", err)
	}

	fmt.Printf("Dashboard regenerated: %s\n", result["path"])
	return nil
}

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
	cmd := NewCommand("maestro status", "maestro status [--json]")
	var jsonOutput bool
	cmd.BoolVar(&jsonOutput, "json", false, "Output status in JSON format")
	if err := cmd.Parse(args); err != nil {
		return err
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
	if err := validate.ProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro status: invalid project name: %w", err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	if err := status.Run(maestroDir, jsonOutput); err != nil {
		return fmt.Errorf("maestro status: %w", err)
	}
	return nil
}

// runDashboard regenerates the dashboard.md file.
func (a *cliApp) runDashboard(args []string) error {
	cmd := NewCommand("maestro dashboard", "maestro dashboard")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("dashboard")
	if err != nil {
		return err
	}

	client := a.createClient(filepath.Join(maestroDir, uds.DefaultSocketName))
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

	path, ok := result["path"]
	if !ok {
		return &CLIError{Code: 1, Msg: "maestro dashboard: response missing path"}
	}
	fmt.Printf("Dashboard regenerated: %s\n", path)
	return nil
}

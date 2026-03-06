package main

import (
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/setup"
)

// runSetup initializes a .maestro/ directory in the given project directory.
func runSetup(args []string) error {
	fs := newFlagSet("maestro setup")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro setup: %v\nusage: maestro setup <project_dir> [project_name]", err)}
	}
	if fs.NArg() < 1 {
		return &CLIError{Code: 1, Msg: "maestro setup: missing project directory\nusage: maestro setup <project_dir> [project_name]"}
	}
	if fs.NArg() > 2 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro setup: unexpected argument: %s\nusage: maestro setup <project_dir> [project_name]", fs.Arg(2))}
	}
	projectDir := fs.Arg(0)
	var projectName string
	if fs.NArg() >= 2 {
		projectName = fs.Arg(1)
	}
	if err := setup.Run(projectDir, projectName); err != nil {
		return fmt.Errorf("maestro setup: %w", err)
	}
	absDir, _ := filepath.Abs(projectDir)
	fmt.Printf("Initialized .maestro/ in %s\n", absDir)
	return nil
}

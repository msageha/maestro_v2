package main

import (
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/setup"
)

// runSetup initializes a .maestro/ directory in the given project directory.
func runSetup(args []string) error {
	cmd := NewCommand("maestro setup", "maestro setup <project_dir> [project_name]")
	if err := cmd.ParsePositional(args); err != nil {
		return err
	}
	if cmd.NArg() < 1 {
		return cmd.UsageErrorf("missing project directory")
	}
	if cmd.NArg() > 2 {
		return cmd.UsageErrorf("unexpected argument: %s", cmd.Arg(2))
	}
	projectDir := cmd.Arg(0)
	var projectName string
	if cmd.NArg() >= 2 {
		projectName = cmd.Arg(1)
	}
	if err := setup.Run(projectDir, projectName); err != nil {
		return fmt.Errorf("maestro setup: %w", err)
	}
	absDir, _ := filepath.Abs(projectDir)
	fmt.Printf("Initialized .maestro/ in %s\n", absDir)
	return nil
}

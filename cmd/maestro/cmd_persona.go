package main

import (
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/daemon/persona"
)

// runPersona dispatches persona subcommands.
func runPersona(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro persona: missing subcommand\nusage: maestro persona <list> [options]"}
	}
	switch args[0] {
	case "list":
		return runPersonaList(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro persona: unknown subcommand: %s\nusage: maestro persona <list> [options]", args[0])}
	}
}

// runPersonaList lists all registered personas.
func runPersonaList(args []string) error {
	cmd := NewCommand("maestro persona list", "maestro persona list")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("persona list")
	if err != nil {
		return err
	}

	personas, err := persona.ListPersonas(filepath.Join(maestroDir, "persona"), nil)
	if err != nil {
		return fmt.Errorf("maestro persona list: %w", err)
	}

	if len(personas) == 0 {
		fmt.Println("No personas found.")
		return nil
	}

	for _, p := range personas {
		desc := p.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Printf("%s\t%s\n", sanitizeForTerminal(p.ID), sanitizeForTerminal(desc))
	}
	return nil
}

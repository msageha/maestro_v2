package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/worker"
)

// runWorker dispatches worker subcommands (currently: standby).
func runWorker(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro worker: missing subcommand\nusage: maestro worker <standby> [options]"}
	}
	switch args[0] {
	case "standby":
		return runWorkerStandby(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro worker: unknown subcommand: %s\nusage: maestro worker standby [options]", args[0])}
	}
}

// runWorkerStandby lists idle workers in JSON format.
func runWorkerStandby(args []string) error {
	fs := newFlagSet("maestro worker standby")
	var modelFilter string
	fs.StringVar(&modelFilter, "model", "", "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro worker standby: %v\nusage: maestro worker standby [--model <model>]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro worker standby: unexpected argument: %s\nusage: maestro worker standby [--model <model>]", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("worker standby")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro worker standby: load config: %w", err)
	}

	output, err := worker.StandbyJSON(worker.StandbyOptions{
		MaestroDir:  maestroDir,
		Config:      cfg,
		ModelFilter: modelFilter,
	})
	if err != nil {
		return fmt.Errorf("maestro worker standby: %w", err)
	}

	fmt.Println(output)
	return nil
}

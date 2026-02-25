package plan

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

type RequestCancelOptions struct {
	CommandID   string
	RequestedBy string
	Reason      string
	MaestroDir  string
	LockMap     *lock.MutexMap
}

func RequestCancel(opts RequestCancelOptions) error {
	if opts.LockMap == nil {
		return fmt.Errorf("LockMap is required")
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	// Check if state file exists (submitted command)
	if sm.StateExists(opts.CommandID) {
		state, err := sm.LoadState(opts.CommandID)
		if err != nil {
			return fmt.Errorf("load state: %w", err)
		}

		if err := SetCancelRequested(state, opts.RequestedBy, opts.Reason); err != nil {
			return err
		}

		if err := sm.SaveState(state); err != nil {
			return fmt.Errorf("save state: %w", err)
		}

		// Also update queue/planner.yaml so daemon's CancelHandler sees the cancel
		if err := setCancelOnPlannerQueue(opts.MaestroDir, opts.CommandID, opts.RequestedBy, opts.Reason); err != nil {
			return fmt.Errorf("update planner queue cancel: %w (state was updated successfully)", err)
		}

		return nil
	}

	// Un-submitted: cancel in queue/planner.yaml
	return cancelInPlannerQueue(opts.MaestroDir, opts.CommandID, opts.RequestedBy, opts.Reason)
}

func setCancelOnPlannerQueue(maestroDir string, commandID string, requestedBy, reason string) error {
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")

	var cq model.CommandQueue
	data, err := readFileIfExists(plannerPath)
	if err != nil {
		return fmt.Errorf("read planner queue: %w", err)
	}
	if data == nil {
		return nil // no queue file, nothing to update
	}

	if err := unmarshalYAML(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			if model.IsTerminal(cq.Commands[i].Status) {
				return nil // already terminal
			}
			cq.Commands[i].CancelReason = &reason
			cq.Commands[i].CancelRequestedAt = &now
			cq.Commands[i].CancelRequestedBy = &requestedBy
			cq.Commands[i].UpdatedAt = now
			return writeYAMLAtomic(plannerPath, cq)
		}
	}

	return nil // command not found in queue — that's ok for submitted commands
}

func cancelInPlannerQueue(maestroDir string, commandID string, requestedBy, reason string) error {
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")

	var cq model.CommandQueue
	data, err := readFileIfExists(plannerPath)
	if err != nil {
		return fmt.Errorf("read planner queue: %w", err)
	}
	if data == nil {
		return fmt.Errorf("command %s not found", commandID)
	}

	if err := unmarshalYAML(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	found := false
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			if model.IsTerminal(cq.Commands[i].Status) {
				return nil // already terminal, idempotent
			}
			cq.Commands[i].Status = model.StatusCancelled
			cq.Commands[i].CancelReason = &reason
			cq.Commands[i].CancelRequestedAt = &now
			cq.Commands[i].CancelRequestedBy = &requestedBy
			cq.Commands[i].LeaseOwner = nil
			cq.Commands[i].LeaseExpiresAt = nil
			cq.Commands[i].UpdatedAt = now
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("command %s not found in planner queue", commandID)
	}

	return writeYAMLAtomic(plannerPath, cq)
}

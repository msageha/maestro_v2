package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runPlanRequestCancel requests cancellation of an active command.
func (a *cliApp) runPlanRequestCancel(args []string) error {
	cmd := NewCommand("maestro plan request-cancel", "maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]")
	var commandID, requestedBy, reason string
	cmd.RequiredString(&commandID, "command-id", "Command ID to cancel")
	cmd.StringVar(&requestedBy, "requested-by", "", "Agent or user who requested cancellation")
	cmd.StringVar(&reason, "reason", "", "Reason for cancellation")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	if requestedBy == "" {
		requestedBy = "cli"
	}

	maestroDir, err := requireMaestroDir("plan request-cancel")
	if err != nil {
		return err
	}

	// Route through daemon UDS to respect single-writer architecture
	params := map[string]any{
		"target":       "planner",
		"type":         "cancel-request",
		"command_id":   commandID,
		"requested_by": requestedBy,
		"reason":       reason,
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		return fmt.Errorf("maestro plan request-cancel: %w", err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan request-cancel: [%s] %s", code, msg)}
	}

	fmt.Printf("cancel requested for command %s\n", commandID)
	return nil
}

// runPlanRebuild rebuilds plan state from existing results.
func (a *cliApp) runPlanRebuild(args []string) error {
	cmd := NewCommand("maestro plan rebuild", "maestro plan rebuild --command-id <id>")
	var commandID string
	cmd.RequiredString(&commandID, "command-id", "Command ID to rebuild state for")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan rebuild")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "rebuild",
		"data": map[string]any{
			"command_id": commandID,
		},
	}

	return a.sendPlanCommand("plan rebuild", maestroDir, params, planCommandTimeout)
}

// runPlanUnquarantine clears quarantine state on a command's integration
// branch so the next queue scan can re-enqueue merge attempts.
//
// F-017 / F-020 design decision (intentionally NOT exposed to Orchestrator
// / Planner agents):
//
// Quarantine is the daemon's last-resort backstop for "3+ consecutive merge
// failures" or "publish_failed past the retry budget". Reaching quarantine
// means the automated retry loop has converged on a failure mode the
// agents themselves did not resolve, so handing the unquarantine button
// back to those same agents would defeat the safety semantics. The reviewer
// suggested either (a) opening unquarantine to Planner or (b) adding a
// high-level Orchestrator wrapper; both options reintroduce the
// "self-healing loop on the very surface that already failed" failure mode
// that the operator-only gate exists to prevent.
//
// What IS available to the agents:
//   - `maestro plan resume-merge`   (Planner / operator) — replays merge
//     attempts after a worker has resolved a conflict.
//   - `maestro plan retry-publish`  (Planner / operator) — retries publish
//     after the cooldown elapses or after a worker resolves a publish_conflict.
//   - `maestro plan resolve-conflict` (Planner / operator) — clears a
//     worker from commit_failed_workers when publish-to-base is blocked by a
//     stale commit failure. NOT the right command for a phase merge_conflict;
//     for those the Planner uses `plan add-task --worker-id <worker>` to
//     dispatch a resolution task and the daemon auto-runs resume_merge once
//     the worker reports.
//
// `unquarantine` remains operator-only, with the reasoning that an operator
// looking at the quarantine state can decide whether the underlying root
// cause has actually been addressed before resetting the failure counters.
func (a *cliApp) runPlanUnquarantine(args []string) error {
	cmd := NewCommand("maestro plan unquarantine", "maestro plan unquarantine --command-id <id> [--reason <text>]")
	var commandID, reason string
	cmd.RequiredString(&commandID, "command-id", "Command ID to unquarantine")
	cmd.StringVar(&reason, "reason", "", "Reason for unquarantine")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	// Multi-layer defense: reject Planner callers at the CLI layer before
	// contacting the daemon. The daemon's plan handler already enforces the
	// same trust boundary (see internal/daemon/plan_handler.go handlePlan),
	// but short-circuiting here ensures Planner-originated invocations fail
	// deterministically even if the daemon check is bypassed or regresses.
	if role := os.Getenv(uds.CallerRoleEnv); role == uds.RolePlanner {
		return &CLIError{
			Code: 1,
			Msg:  "maestro plan unquarantine: restricted to operator role; Planner is not permitted (multi-layer defense)",
		}
	}

	maestroDir, err := requireMaestroDir("plan unquarantine")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "unquarantine",
		"data": map[string]any{
			"command_id": commandID,
			"reason":     reason,
		},
	}
	return a.sendPlanCommand("plan unquarantine", maestroDir, params, planCommandTimeout)
}

// runPlanResumeMerge resets the merge failure counter and moves a stuck
// integration (conflict / partial_merge / failed) back to a re-mergeable state.
func (a *cliApp) runPlanResumeMerge(args []string) error {
	cmd := NewCommand("maestro plan resume-merge", "maestro plan resume-merge --command-id <id>")
	var commandID string
	cmd.RequiredString(&commandID, "command-id", "Command ID to resume merge for")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan resume-merge")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "resume_merge",
		"data": map[string]any{
			"command_id": commandID,
		},
	}
	return a.sendPlanCommand("plan resume-merge", maestroDir, params, planCommandTimeout)
}

// runPlanRecover asks the daemon to choose the appropriate recovery action for
// the command's current worktree integration state.
func (a *cliApp) runPlanRecover(args []string) error {
	cmd := NewCommand("maestro plan recover", "maestro plan recover --command-id <id>")
	var commandID string
	cmd.RequiredString(&commandID, "command-id", "Command ID to recover")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan recover")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "auto_recover",
		"data": map[string]any{
			"command_id": commandID,
		},
	}
	return a.sendPlanCommand("plan recover", maestroDir, params, planCommandTimeout)
}

// runPlanRetryPublish resets publish failure state and transitions the
// integration back to merged so the next scan re-attempts publish-to-base.
func (a *cliApp) runPlanRetryPublish(args []string) error {
	cmd := NewCommand("maestro plan retry-publish", "maestro plan retry-publish --command-id <id>")
	var commandID string
	cmd.RequiredString(&commandID, "command-id", "Command ID to retry publish for")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan retry-publish")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "retry_publish",
		"data": map[string]any{
			"command_id": commandID,
		},
	}
	return a.sendPlanCommand("plan retry-publish", maestroDir, params, planCommandTimeout)
}

// runResolveConflict clears a worker from CommitFailedWorkers in the
// command's worktree state. This is the recovery path for **publish-blocking
// commit failures** (where the worker's worktree could not be committed
// during integration), NOT for in-phase merge_conflict signals.
//
// 2026-04-28 E2E follow-up: an operator-style invocation by the Planner on a
// regular merge_conflict was rejected with
//
//	plan_resolve_conflict error=integration is already resolved:
//	worker worker2 is not in commit_failed_workers ...
//
// because phase merge_conflicts are surfaced via the merge_conflict signal
// (Planner → `plan add-task --worker-id <worker>` for the resolution task,
// then the daemon's AutoRecoverAfterResolution auto-fires resume_merge).
// This command's job is the narrower "the worker has been verified back to
// a publishable state, take it off the commit-failed gating list so
// publish-to-base can proceed" — the wording in the help / docs is now
// explicit about that scope so future Planner / operator runs do not pick
// the wrong recovery command for a normal merge conflict.
//
// Usage:
//
//	maestro plan resolve-conflict \
//	    --command-id   <id>           # parent command id
//	    --phase-id     <id>           # phase whose publish-to-base is blocked
//	    --worker-id    <id>           # worker pinned in commit_failed_workers
//	    [--conflicting-files <list>]  # repeat or comma-separated; optional hint
//
// Example:
//
//	maestro plan resolve-conflict --command-id cmd_42 --phase-id ph_3 \
//	    --worker-id worker2 --conflicting-files internal/a.go,internal/b.go
//
// For a normal phase merge_conflict, the Planner instead calls:
//
//	maestro plan add-task --command-id cmd_42 --worker-id worker2 \
//	    --purpose "resolve merge conflict in <files>" ...
func (a *cliApp) runResolveConflict(args []string) error {
	cmd := NewCommand("maestro plan resolve-conflict",
		"maestro plan resolve-conflict --command-id <id> --phase-id <id> --worker-id <id> "+
			"[--conflicting-files <path>[,<path>...]]... "+
			"# clears the worker from commit_failed_workers (publish-blocking); "+
			"NOT for phase merge_conflict — use 'plan add-task --worker-id <id>' for those")
	var commandID, phaseID, workerID string
	var conflictingFiles stringSliceFlag
	cmd.RequiredString(&commandID, "command-id", "parent command id")
	cmd.RequiredString(&phaseID, "phase-id", "phase id containing the conflict")
	cmd.RequiredString(&workerID, "worker-id", "worker id whose worktree conflicts")
	cmd.Var(&conflictingFiles, "conflicting-files", "conflicting file paths (repeat flag or comma-separated)")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	if err := validate.PhaseID(phaseID); err != nil {
		return cmd.Errorf("invalid --phase-id: %v", err)
	}
	if err := validate.ID(workerID); err != nil {
		return cmd.Errorf("invalid --worker-id: %v", err)
	}

	// Allow comma-separated values inside a single --conflicting-files flag
	// in addition to repeated flag invocations. Empty entries are dropped so
	// "--conflicting-files a.go," does not propagate a blank path.
	files := make([]string, 0, len(conflictingFiles))
	for _, raw := range conflictingFiles {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				files = append(files, p)
			}
		}
	}

	maestroDir, err := requireMaestroDir("resolve-conflict")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "resolve_conflict",
		"data": map[string]any{
			"command_id":        commandID,
			"phase_id":          phaseID,
			"worker_id":         workerID,
			"conflicting_files": files,
		},
	}
	return a.sendPlanCommand("resolve-conflict", maestroDir, params, planCommandTimeout)
}

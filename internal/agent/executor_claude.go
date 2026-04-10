package agent

import (
	"context"
	"fmt"
)

// --- Claude Process Recovery ---

// ensureClaudeRunning checks whether the pane is running a shell (indicating
// Claude has crashed or exited) and re-launches Claude if necessary.
// This guards against the scenario where Claude crashes back to the shell
// but the pane PID stays the same (shell PID unchanged), which
// DetectProcessRestart cannot detect.
// Returns nil if Claude is confirmed running, or a retryable error on failure.
func (e *Executor) ensureClaudeRunning(ctx context.Context, paneTarget, agentID string) error {
	cmd, err := e.paneIO.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		e.log(logLevelWarn, "ensure_claude_running_check_failed agent_id=%s error=%v", agentID, err)
		// Cannot determine state; proceed optimistically
		return nil
	}

	if !e.paneIO.IsShellCommand(cmd) {
		// Not a shell — Claude (or another process) is running
		return nil
	}

	e.log(logLevelWarn, "claude_not_running agent_id=%s pane_command=%s, re-launching", agentID, cmd)

	// Reset clear_ready since we are starting a fresh Claude session
	if resetErr := e.paneState.ResetClearReady(paneTarget); resetErr != nil {
		e.log(logLevelError, "ensure_claude_running_reset_clear_ready agent_id=%s error=%v", agentID, resetErr)
	}

	// Re-launch Claude
	if sendErr := e.paneIO.SendCommand(paneTarget, "maestro agent launch"); sendErr != nil {
		return fmt.Errorf("ensureClaudeRunning: re-launch: %w", sendErr)
	}

	// Wait for Claude prompt readiness (fail-closed)
	launchCtx, cancel := context.WithTimeout(ctx, e.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if waitErr := e.waitReadyStrict(launchCtx, paneTarget); waitErr != nil {
		return fmt.Errorf("ensureClaudeRunning: wait for Claude ready: %w", waitErr)
	}

	e.log(logLevelInfo, "claude_relaunched agent_id=%s", agentID)
	return nil
}

// --- Working Directory Management ---

// ensureWorkingDir ensures the agent's Claude Code process is running in the
// specified working directory. If the current CWD (tracked via @cwd tmux user
// variable) differs from workingDir, the method exits the current Claude
// process, changes the shell CWD, and re-launches Claude.
//
// This is called transparently by the executor before task delivery, so that
// Workers run in their worktree directory without being aware of worktrees.
func (e *Executor) ensureWorkingDir(ctx context.Context, paneTarget, workingDir string) error {
	if workingDir == "" {
		return nil
	}

	// Validate path: reject control characters to prevent injection via SendCommand
	if containsControlChars(workingDir) {
		return fmt.Errorf("ensureWorkingDir: working dir contains control characters: %q", workingDir)
	}

	// Check current CWD from tmux user variable
	currentCWD := e.paneState.GetCWD(paneTarget)
	if currentCWD == workingDir {
		e.log(logLevelDebug, "working_dir unchanged cwd=%s", workingDir)
		return nil
	}

	e.log(logLevelInfo, "working_dir_change old=%q new=%q", currentCWD, workingDir)

	// Step 1: Kill the current pane process and respawn a fresh shell in the
	// target working directory. This replaces the fragile Ctrl+C → Ctrl+D →
	// waitForShell → cd sequence which broke when Claude did not exit cleanly.
	if err := e.paneIO.RespawnPane(paneTarget, workingDir); err != nil {
		return fmt.Errorf("ensureWorkingDir: respawn pane: %w", err)
	}

	// Step 2: Wait for the fresh shell to be ready
	if err := e.waitForShell(ctx, paneTarget); err != nil {
		return fmt.Errorf("ensureWorkingDir: wait for shell after respawn: %w", err)
	}

	// Step 3: Re-launch Claude
	if err := e.paneIO.SendCommand(paneTarget, "maestro agent launch"); err != nil {
		return fmt.Errorf("ensureWorkingDir: re-launch: %w", err)
	}

	// Step 4: Wait for Claude prompt readiness (fail-closed: error on timeout)
	// Use a generous timeout since Claude startup can take a few seconds.
	launchCtx, cancel := context.WithTimeout(ctx, e.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if err := e.waitReadyStrict(launchCtx, paneTarget); err != nil {
		return fmt.Errorf("ensureWorkingDir: wait for Claude ready: %w", err)
	}

	// Step 5: Update CWD tracking and reset clear_ready state
	if err := e.paneState.SetCWD(paneTarget, workingDir); err != nil {
		e.log(logLevelWarn, "set_cwd_failed cwd=%s error=%v", workingDir, err)
	}
	// Reset clear_ready since we started a fresh Claude session
	if err := e.paneState.ResetClearReady(paneTarget); err != nil {
		e.log(logLevelError, "reset_clear_ready_failed error=%v", err)
		return fmt.Errorf("ensureWorkingDir: reset_clear_ready: %w", err)
	}

	e.log(logLevelInfo, "working_dir_changed cwd=%s", workingDir)
	return nil
}

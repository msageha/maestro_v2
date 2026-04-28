package formation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// trustDialogPanesFile is the filename where ready pane targets are persisted
// so the daemon process can continue auto-accepting the trust dialog after the
// CLI process exits.
const trustDialogPanesFile = "trust_dialog_panes.txt"

// createFormation creates the tmux session with orchestrator, planner, and worker windows.
// On partial failure, it rolls back by killing the tmux session.
func createFormation(maestroDir string, cfg model.Config) (retErr error) {
	// Kill existing session if any
	if tmux.SessionExists() {
		slog.Debug("createFormation: killing pre-existing session before creation")
		if err := tmux.KillSession(); err != nil {
			return fmt.Errorf("kill existing session: %w", err)
		}
	}

	// Create the session and apply server-level hardening atomically in a
	// single tmux invocation. Chaining avoids both:
	//   - the "empty server exits before we can disable exit-empty" race when
	//     set-option is issued before any session exists, and
	//   - the "user's tmux.conf exit-unattached=on destroys the session
	//     between new-session and the follow-up set-option" race.
	slog.Debug("createFormation: creating session with server hardening (atomic)")

	if err := tmux.CreateSessionWithServerOptions("orchestrator", map[string]string{
		"exit-empty":      "off",
		"exit-unattached": "off",
	}); err != nil {
		if errors.Is(err, tmux.ErrTmuxServer) {
			return fmt.Errorf("create session with server options: %w; tmux server is unavailable, and this often means the current sandbox or container blocks tmux socket creation", err)
		}
		return fmt.Errorf("create session with server options: %w", err)
	}

	// Rollback: if any subsequent step fails, destroy the partially-created session
	defer func() {
		if retErr != nil {
			slog.Warn("createFormation: rolling back due to error", "error", retErr)
			if tmux.SessionExists() {
				if killErr := tmux.KillSession(); killErr != nil {
					slog.Warn("createFormation: rollback kill session failed", "error", killErr)
				}
			}
		}
	}()

	// Harden session: prevent user-level tmux config from destroying the detached session.
	if err := tmux.SetSessionOption("destroy-unattached", "off"); err != nil {
		return fmt.Errorf("set destroy-unattached: %w", err)
	}

	// Set remain-on-exit for orchestrator window.
	orchWindow := fmt.Sprintf("=%s:0", tmux.GetSessionName())
	if err := tmux.SetWindowOption(orchWindow, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("set remain-on-exit for orchestrator: %w", err)
	}

	orchPane := fmt.Sprintf("%s:0.0", tmux.GetSessionName())
	orchRuntime, orchModel := model.ParseRuntimeFromModel(resolveModel(cfg, "orchestrator"))
	if err := setAgentVars(orchPane, "orchestrator", "orchestrator", orchModel, orchRuntime); err != nil {
		return err
	}

	// Window 1: planner
	if err := tmux.CreateWindow("planner"); err != nil {
		return fmt.Errorf("create planner window: %w", err)
	}

	// Set remain-on-exit for planner window
	plannerWindow := fmt.Sprintf("=%s:1", tmux.GetSessionName())
	if err := tmux.SetWindowOption(plannerWindow, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("set remain-on-exit for planner: %w", err)
	}

	plannerPane := fmt.Sprintf("%s:1.0", tmux.GetSessionName())
	plannerRuntime, plannerModel := model.ParseRuntimeFromModel(resolveModel(cfg, "planner"))
	if err := setAgentVars(plannerPane, "planner", "planner", plannerModel, plannerRuntime); err != nil {
		return err
	}

	// Window 2: workers
	workerCount := max(cfg.Agents.Workers.Count, 1)

	if err := tmux.CreateWindow("workers"); err != nil {
		return fmt.Errorf("create workers window: %w", err)
	}

	// Set remain-on-exit for workers window
	workerWindow := fmt.Sprintf("=%s:2", tmux.GetSessionName())
	if err := tmux.SetWindowOption(workerWindow, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("set remain-on-exit for workers: %w", err)
	}

	// Limit scrollback buffer for worker panes to reduce memory usage
	if err := tmux.SetWindowOption(workerWindow, "history-limit", "500"); err != nil {
		return fmt.Errorf("set history-limit for workers: %w", err)
	}

	panes, err := tmux.SetupWorkerGrid(workerWindow, workerCount)
	if err != nil {
		return fmt.Errorf("setup worker grid: %w", err)
	}

	for i, pane := range panes {
		agentID := fmt.Sprintf("worker%d", i+1)
		workerRuntime, workerModel := model.ParseRuntimeFromModel(resolveModel(cfg, agentID))
		if err := setAgentVars(pane, agentID, "worker", workerModel, workerRuntime); err != nil {
			return err
		}
	}

	// Launch agents in each pane
	allPanes := make([]string, 0, 2+cfg.Agents.Workers.Count)
	allPanes = append(allPanes, orchPane, plannerPane)
	allPanes = append(allPanes, panes...)

	// Wait for each pane's shell to be ready before sending commands.
	// Orchestrator + planner are required; worker panes are best-effort
	// with automatic cleanup on failure.
	requiredPanes := allPanes[:2] // orchestrator + planner
	optionalPanes := allPanes[2:] // workers
	timeout := shellReadyTimeout(cfg)
	readyPanes, err := preparePanes(requiredPanes, optionalPanes, timeout, waitForShellReady, killPaneByTarget)
	if err != nil {
		return err
	}

	// Use the absolute path of the current binary to avoid version skew: the pane
	// shell's PATH may resolve a different (older) maestro binary than the one
	// that started this formation, which would break flags added in newer versions.
	launchCmd := agent.ResolvedLaunchCommand()
	for _, pane := range readyPanes {
		if err := tmux.SendCommand(pane, launchCmd); err != nil {
			return fmt.Errorf("launch agent in %s: %w", pane, err)
		}
	}

	// Persist ready pane targets to file so the daemon process (which outlives
	// this CLI process) can continue auto-accepting the trust dialog.
	// The trust dialog often appears 30+ seconds after launch, well after the
	// CLI process exits, so the daemon must drive the auto-accept goroutine.
	if err := writeTrustDialogPanesFile(maestroDir, readyPanes); err != nil {
		slog.Warn("createFormation: could not write trust dialog panes file", "error", err)
	}

	// Auto-accept Claude Code startup dialogs.
	// Claude Code has no env var or CLI flag to skip the trust dialog.
	// --dangerously-skip-permissions only covers per-tool permission checks
	// and may show a Bypass Permissions confirmation. See the function doc
	// comment for the key selection rules.
	// NOTE: This goroutine runs in the CLI process which exits shortly after
	// formation is complete. The daemon calls StartTrustDialogAcceptor to cover
	// the full window after CLI exits.
	autoAcceptTrustDialog(readyPanes)

	// Verify that agents did not exit immediately after launch. This is
	// synchronous so `maestro up --detach` does not report success when agent
	// panes have already fallen back to the shell.
	if err := checkAgentsLaunched(readyPanes); err != nil {
		return err
	}

	// Select orchestrator window so `tmux attach` lands there
	if err := tmux.SelectWindow(fmt.Sprintf("=%s:0", tmux.GetSessionName())); err != nil {
		return fmt.Errorf("select orchestrator window: %w", err)
	}

	return nil
}

// waitForShellReady polls a tmux pane until its current command is a known
// shell AND the shell has confirmed readiness via a sentinel echo probe.
//
// Two-phase approach:
//  1. Poll pane_current_command until a shell is detected.
//  2. Send a sentinel echo command and wait for its output to appear.
//
// Phase 2 catches the common race where pane_current_command shows the shell
// while .zshrc/.bashrc is still initialising (conda init, pyenv, NVM, etc.).
// Without this, a send-keys with "maestro agent launch" may be queued in the
// terminal buffer and run only after shell init completes — or, if rc init
// reads stdin, may be silently consumed by the init script. The sentinel probe
// guarantees the shell is accepting interactive input before the caller sends
// the real launch command.
func waitForShellReady(ctx context.Context, pane string) error {
	const maxConsecutiveErrors = 5
	consecutiveErrors := 0
	var lastErr error

	// Phase 1: wait for pane_current_command to show a shell.
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waitForShellReady cancelled: %w", err)
		}
		cmd, err := tmux.GetPaneCurrentCommand(pane)
		if err != nil {
			consecutiveErrors++
			lastErr = err
			slog.Warn("GetPaneCurrentCommand failed", "pane", pane, "attempt", consecutiveErrors, "max_attempts", maxConsecutiveErrors, "error", err)
			if consecutiveErrors >= maxConsecutiveErrors {
				return fmt.Errorf("waitForShellReady: %d consecutive errors, last: %w", consecutiveErrors, lastErr)
			}
		} else {
			consecutiveErrors = 0
			if tmux.IsShellCommand(cmd) {
				break
			}
		}
		t := time.NewTimer(100 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("waitForShellReady cancelled: %w", ctx.Err())
		}
	}

	// Phase 2: confirm the shell has finished rc/init by probing with a
	// sentinel echo command. This is fail-open: if the probe cannot be
	// confirmed before the context expires, we log a warning and proceed
	// rather than failing formation.
	confirmShellInteractive(ctx, pane)
	return nil
}

// confirmShellInteractive sends a sentinel echo command to the pane and waits
// for its output to appear, proving the shell has finished its init sequence
// and is accepting interactive input.
//
// Fail-open: if the sentinel cannot be detected before ctx expires (e.g. an
// unusually slow .zshrc), a warning is logged and the function returns without
// error so formation can still proceed.
func confirmShellInteractive(ctx context.Context, pane string) {
	sentinel := fmt.Sprintf("__MAESTRO_RDY_%d__", time.Now().UnixNano())

	// Send the sentinel echo. If this fails the shell is likely not ready at
	// all; log and bail out (the subsequent LaunchCommand send will also fail,
	// which is surfaced via the agent monitor).
	if err := tmux.SendCommand(pane, "echo "+sentinel); err != nil {
		slog.Warn("confirmShellInteractive: sentinel send failed, proceeding",
			"pane", pane, "error", err)
		return
	}

	// Poll CapturePane (primary screen) for the sentinel output.
	const pollInterval = 150 * time.Millisecond
	for {
		if ctx.Err() != nil {
			slog.Warn("confirmShellInteractive: sentinel not detected before timeout, proceeding",
				"pane", pane)
			return
		}

		// Capture the last 15 lines — enough to catch the sentinel output
		// even if a PS1 prompt or MOTD follows it.
		content, err := tmux.CapturePane(pane, 15)
		if err == nil && strings.Contains(content, sentinel) {
			slog.Debug("confirmShellInteractive: sentinel detected", "pane", pane)
			return
		}

		t := time.NewTimer(pollInterval)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			slog.Warn("confirmShellInteractive: sentinel not detected before timeout, proceeding",
				"pane", pane)
			return
		}
	}
}

func setAgentVars(pane, agentID, role, agentModel, agentRuntime string) error {
	// Use a slice (not map) to guarantee deterministic set order and to ensure
	// @runtime is always written last. Writing @runtime last is critical for
	// non-claude-code runtimes where agentModel is empty: some tmux versions
	// treat set-option with an empty value as an unset-option (removing the
	// option from the pane's option table), which may disturb previously written
	// options. Setting @runtime after @model avoids any cross-option interference.
	vars := []struct{ k, v string }{
		{"agent_id", agentID},
		{"role", role},
		{"status", "idle"},
		{"model", agentModel},
		{"runtime", agentRuntime},
	}
	for _, kv := range vars {
		if kv.v == "" {
			// Skip empty-value options: tmux's set-option behavior with an empty
			// string value is version-dependent — on some builds it silently
			// removes the option instead of storing an empty string. Skipping is
			// safe because GetUserVar returns "" for unset options, matching the
			// same value readPaneVars would see.
			continue
		}
		if err := tmux.SetUserVar(pane, kv.k, kv.v); err != nil {
			return fmt.Errorf("set @%s on %s: %w", kv.k, pane, err)
		}
	}
	return nil
}

const defaultShellReadyTimeoutSec = 10

// shellReadyTimeout returns the configured shell readiness timeout or the default (10s).
func shellReadyTimeout(cfg model.Config) time.Duration {
	if cfg.Watcher.ShellReadyTimeoutSec > 0 {
		return time.Duration(cfg.Watcher.ShellReadyTimeoutSec) * time.Second
	}
	return defaultShellReadyTimeoutSec * time.Second
}

// killPaneByTarget kills a tmux pane by its target identifier.
// Best-effort: errors are logged but not propagated.
// Package-level variable for test overriding.
var killPaneByTarget = func(pane string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "tmux", "kill-pane", "-t", pane).Run(); err != nil { //nolint:gosec // "tmux" is a fixed command; pane target is derived from internal formation setup
		slog.Warn("kill-pane failed", "pane", pane, "error", err)
	}
}

// preparePanes waits for panes to become ready with automatic cleanup of failed panes.
// Required panes (orchestrator + planner) must all succeed; optional panes (workers)
// are killed on failure and excluded from the result. Returns the list of ready panes.
func preparePanes(
	requiredPanes []string,
	optionalPanes []string,
	timeout time.Duration,
	waitFn func(ctx context.Context, pane string) error,
	killFn func(pane string),
) ([]string, error) {
	readyPanes := make([]string, 0, len(requiredPanes)+len(optionalPanes))

	// Required panes must all succeed
	for _, pane := range requiredPanes {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := waitFn(ctx, pane)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("required pane %s not ready: %w", pane, err)
		}
		readyPanes = append(readyPanes, pane)
	}

	// Optional panes: cleanup on failure, continue with remaining
	var failedCount int
	for _, pane := range optionalPanes {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := waitFn(ctx, pane)
		cancel()
		if err != nil {
			slog.Warn("worker pane not ready, cleaning up", "pane", pane, "error", err)
			killFn(pane)
			failedCount++
			continue
		}
		readyPanes = append(readyPanes, pane)
	}

	if failedCount > 0 {
		slog.Warn("formation started with partial workers",
			"ready_workers", len(readyPanes)-len(requiredPanes),
			"failed_workers", failedCount)
	}

	return readyPanes, nil
}

// trustDialogWindow is the total duration over which the auto-accept goroutine
// sends Enter keystrokes to each pane. Two minutes covers even the slowest
// startups (Claude Code binary initialization, network auth, model handshake).
const trustDialogWindow = 2 * time.Minute

// trustDialogSendInterval is the gap between successive Enter batches.
// Three seconds ensures the dialog is accepted within one interval of
// appearing, with minimal goroutine activity.
const trustDialogSendInterval = 3 * time.Second

// autoAcceptTrustDialog periodically sends startup-dialog keystrokes to every
// pane for trustDialogWindow to auto-accept Claude Code's workspace trust dialog
// ("Is this a project you created or one you trust?") and the Bypass
// Permissions confirmation shown by recent Claude Code releases.
//
// Historically, workspace-trust Enter sends were unconditional — no pane-content
// detection was used — for three reasons discovered through production failures:
//
//  1. Detection via tmux capture-pane is unreliable for this dialog.
//     Claude Code's Ink-based TUI renders on the terminal's alternate screen.
//     capture-pane without -a reads the primary screen buffer and misses the
//     dialog entirely; with -a it reads the alternate screen but Ink redraws
//     aggressively and the resulting plain-text frame is not stable enough
//     for substring matching. Every detection iteration silently failed while
//     the dialog sat on screen.
//
//  2. Claude Code ignores empty Enter at its interactive prompt (no API call
//     is made), so sends that arrive after dialog acceptance are harmless.
//
//  3. Unconditional periodic sends guarantee acceptance within one
//     trustDialogSendInterval of the dialog appearing, across all startup
//     speeds and terminal rendering behaviors.
//
// Bypass Permissions changes that tradeoff: the confirmation's default
// selection is "No, exit", so Enter alone terminates the agent. Managed panes
// therefore only receive keys when a known startup dialog marker is visible:
// "2" + Enter for Bypass Permissions, or Enter for workspace trust. This avoids
// buffered Enter keystrokes choosing "No, exit" before detection.
func autoAcceptTrustDialog(panes []string) {
	go func() {
		deadline := time.Now().Add(trustDialogWindow)
		for time.Now().Before(deadline) {
			time.Sleep(trustDialogSendInterval)
			for _, pane := range panes {
				if err := sendStartupDialogKeys(pane); err != nil {
					slog.Debug("autoAcceptTrustDialog: send startup dialog keys failed",
						"pane", pane, "error", err)
				}
			}
		}
		slog.Debug("autoAcceptTrustDialog: window closed",
			"panes_count", len(panes), "window", trustDialogWindow)
	}()
}

const bypassPermissionsDialogMarker = "Bypass Permissions mode"
const workspaceTrustDialogMarker = "project you created or one you trust"

func sendStartupDialogKeys(pane string) error {
	role, err := tmux.GetUserVar(pane, "role")
	if err != nil {
		role = ""
	}
	content, err := captureStartupDialogContent(pane)
	if err != nil {
		if isManagedAgentRole(role) {
			return nil
		}
		return tmux.SendKeys(pane, "Enter")
	}
	keys := startupDialogKeys(role, content)
	if len(keys) == 0 {
		return nil
	}
	return tmux.SendKeys(pane, keys...)
}

func startupDialogKeys(role, content string) []string {
	normalized := normalizeStartupDialogContent(content)
	if startupDialogContentReady(normalized) {
		return nil
	}
	if strings.Contains(normalized, bypassPermissionsDialogMarker) {
		return []string{"2", "Enter"}
	}
	if strings.Contains(normalized, workspaceTrustDialogMarker) {
		return []string{"Enter"}
	}
	if isManagedAgentRole(role) {
		return nil
	}
	// Unknown legacy panes are not expected to use Bypass Permissions, so
	// periodic Enter preserves the previous best-effort trust-dialog behavior.
	return []string{"Enter"}
}

func isManagedAgentRole(role string) bool {
	return role == "orchestrator" || role == "planner" || role == "worker"
}

func startupDialogVisible(content string) bool {
	normalized := normalizeStartupDialogContent(content)
	if startupDialogContentReady(normalized) {
		return false
	}
	return strings.Contains(normalized, bypassPermissionsDialogMarker) ||
		strings.Contains(normalized, workspaceTrustDialogMarker)
}

func normalizeStartupDialogContent(content string) string {
	return strings.Join(strings.Fields(content), " ")
}

func startupDialogContentReady(normalized string) bool {
	return strings.Contains(strings.ToLower(normalized), "bypass permissions on")
}

func captureStartupDialogContent(pane string) (string, error) {
	content, err := tmux.CapturePaneJoined(pane, 80)
	if err != nil {
		return "", err
	}
	if alternate, altErr := tmux.CapturePaneAlternateJoined(pane, 80); altErr == nil && alternate != "" {
		content += "\n" + alternate
	}
	return content, nil
}

// writeTrustDialogPanesFile writes pane targets to maestroDir/trust_dialog_panes.txt,
// one pane per line. The daemon reads this file to know which panes to send Enter to.
func writeTrustDialogPanesFile(maestroDir string, panes []string) error {
	path := filepath.Join(maestroDir, trustDialogPanesFile)
	// path is built from a controlled application directory + constant filename.
	f, err := os.Create(path) //nolint:gosec // controlled path
	if err != nil {
		return fmt.Errorf("create %s: %w", trustDialogPanesFile, err)
	}
	defer func() { _ = f.Close() }() // write errors above are reported; close-only failures are acceptable
	for _, p := range panes {
		if _, err := fmt.Fprintln(f, p); err != nil {
			return fmt.Errorf("write pane %s: %w", p, err)
		}
	}
	return nil
}

// StartTrustDialogAcceptor reads the pane list from maestroDir/trust_dialog_panes.txt
// and launches the auto-accept goroutine. Intended to be called from the long-lived
// daemon process so that the trust dialog is accepted even after the CLI process exits.
// No-op if the file does not exist (non-worktree mode or already handled).
func StartTrustDialogAcceptor(maestroDir string) {
	path := filepath.Join(maestroDir, trustDialogPanesFile)
	// path is built from a controlled application directory + constant filename.
	f, err := os.Open(path) //nolint:gosec // controlled path
	if err != nil {
		// File absent is expected when running without a formation (e.g. tests).
		return
	}
	defer func() { _ = f.Close() }() // read-only handle: close error is irrelevant

	var panes []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if p := strings.TrimSpace(scanner.Text()); p != "" {
			panes = append(panes, p)
		}
	}
	if len(panes) == 0 {
		return
	}
	// len(panes) is an int derived from filtered slog input; there is no
	// tainted string here, so gosec G706's log-injection warning does not
	// apply.
	slog.Info("daemon: starting trust dialog acceptor", "panes_count", len(panes)) //nolint:gosec // panes_count is an int, not a tainted string
	autoAcceptTrustDialog(panes)
}

// checkAgentsLaunched polls panes after sending the launch command to detect
// immediate startup failures. If an agent binary exits immediately (e.g. due
// to an auth error, signal, or missing configuration), the pane reverts to the
// shell prompt. This function returns an error for any pane that remains dead
// after the polling window.
//
// Timing: polls start 5 s after launch (enough for the first startup-dialog
// auto-accept pass to run and for fast failures to return to the shell) and
// continue every 2 s for about a minute. Claude Code startup dialogs can remain
// visible for tens of seconds while trust / bypass-permissions state is being
// persisted, especially when four worker panes start at once. The check must
// therefore outlive the common first-start dialog window instead of rolling the
// formation back while auto-accept is still making progress.
//
// The daemon's ensureClaudeRunning provides ongoing recovery after a healthy
// launch; this check keeps startup fail-closed when agents exit immediately.
func checkAgentsLaunched(panes []string) error {
	const (
		initialDelay = 5 * time.Second
		pollInterval = 2 * time.Second
		maxAttempts  = 30
	)

	time.Sleep(initialDelay)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var deadPanes []string
		var startupDialogPanes []string
		for _, pane := range panes {
			cmd, err := tmux.GetPaneCurrentCommand(pane)
			if err != nil {
				slog.Debug("checkAgentsLaunched: pane query failed", "pane", pane, "error", err)
				continue
			}
			if tmux.IsShellCommand(cmd) {
				deadPanes = append(deadPanes, pane)
				slog.Warn("checkAgentsLaunched: agent exited immediately after launch",
					"pane", pane,
					"shell_command", cmd,
					"hint", "check agent logs or run 'maestro status' for details")
			}
			if content, err := captureStartupDialogContent(pane); err == nil && startupDialogVisible(content) {
				startupDialogPanes = append(startupDialogPanes, pane)
				if sendErr := sendStartupDialogKeys(pane); sendErr != nil {
					slog.Debug("checkAgentsLaunched: startup dialog key send failed", "pane", pane, "error", sendErr)
				}
				slog.Warn("checkAgentsLaunched: agent still waiting at startup dialog",
					"pane", pane,
					"hint", "startup dialog auto-accept is still pending")
			}
		}

		if len(deadPanes) == 0 && len(startupDialogPanes) == 0 {
			// All agents are running; no further polling needed.
			slog.Debug("checkAgentsLaunched: all agents running", "panes_count", len(panes), "attempt", attempt)
			return nil
		}

		if attempt < maxAttempts-1 {
			time.Sleep(pollInterval)
			continue
		}

		var reasons []string
		if len(deadPanes) > 0 {
			reasons = append(reasons, "returned to shell: "+strings.Join(deadPanes, ", "))
		}
		if len(startupDialogPanes) > 0 {
			reasons = append(reasons, "stuck at startup dialog: "+strings.Join(startupDialogPanes, ", "))
		}
		return fmt.Errorf("agent launch failed: panes %s", strings.Join(reasons, "; "))
	}

	return nil
}

// resolveModel determines the model for a given agent.
func resolveModel(cfg model.Config, agentID string) string {
	switch agentID {
	case "orchestrator":
		if m := cfg.Agents.Orchestrator.Model; m != "" {
			return m
		}
	case "planner":
		if m := cfg.Agents.Planner.Model; m != "" {
			return m
		}
	default:
		return model.ResolveWorkerModel(agentID, cfg)
	}
	return "sonnet"
}

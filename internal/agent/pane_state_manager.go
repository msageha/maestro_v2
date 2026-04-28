package agent

import (
	"errors"
	"fmt"
)

// paneStateManager centralizes management of tmux user variables that track
// pane state (clear_ready, clear_ready_pid, cwd, status). This extracts the
// scattered Get/Set/Reset logic from Executor into a single responsible type.
//
// All state is stored as tmux user variables (@clear_ready, @clear_ready_pid,
// @cwd, @status) on the target pane. paneStateManager is stateless itself —
// it delegates all reads/writes to PaneIO.
type paneStateManager struct {
	paneIO PaneIO
}

// newPaneStateManager creates a paneStateManager backed by the given PaneIO.
func newPaneStateManager(paneIO PaneIO) *paneStateManager {
	return &paneStateManager{paneIO: paneIO}
}

// IsClearReady returns whether the pane has an active conversation and is
// ready for /clear. Returns false if the variable is unset or on error.
func (m *paneStateManager) IsClearReady(paneTarget string) bool {
	v, err := m.paneIO.GetUserVar(paneTarget, "clear_ready")
	return err == nil && v == "true"
}

// SetClearReady marks the pane as having an active conversation (clear_ready=true)
// and records the current PID for restart detection.
func (m *paneStateManager) SetClearReady(paneTarget, pid string) error {
	if err := m.paneIO.SetUserVar(paneTarget, "clear_ready", "true"); err != nil {
		return fmt.Errorf("set clear_ready: %w", err)
	}
	if pid != "" {
		if err := m.paneIO.SetUserVar(paneTarget, "clear_ready_pid", pid); err != nil {
			return fmt.Errorf("set clear_ready_pid: %w", err)
		}
	}
	return nil
}

// ResetClearReady clears both clear_ready and clear_ready_pid.
// Used when a /clear fails, the process restarts, or the working directory changes.
// Both errors are collected and returned via errors.Join so neither is discarded.
func (m *paneStateManager) ResetClearReady(paneTarget string) error {
	var err1, err2 error
	if err := m.paneIO.SetUserVar(paneTarget, "clear_ready", ""); err != nil {
		err1 = fmt.Errorf("reset clear_ready: %w", err)
	}
	if err := m.paneIO.SetUserVar(paneTarget, "clear_ready_pid", ""); err != nil {
		err2 = fmt.Errorf("reset clear_ready_pid: %w", err)
	}
	return errors.Join(err1, err2)
}

// DetectProcessRestart checks if the pane's process PID has changed since
// clear_ready_pid was stored. Returns true if a restart was detected and
// clear_ready was reset.
func (m *paneStateManager) DetectProcessRestart(paneTarget string) (restarted bool, currentPID string, err error) {
	currentPID, err = m.paneIO.GetPanePID(paneTarget)
	if err != nil {
		return false, "", fmt.Errorf("get pane pid: %w", err)
	}

	storedPID, _ := m.paneIO.GetUserVar(paneTarget, "clear_ready_pid")
	if storedPID != "" && storedPID != currentPID {
		// Process restarted — reset clear_ready.
		// Reset errors are non-fatal: the caller should still know a restart
		// occurred even if the reset partially failed.
		resetErr := m.ResetClearReady(paneTarget)
		return true, currentPID, resetErr
	}

	return false, currentPID, nil
}

// SetStatus sets the @status user variable on the pane.
func (m *paneStateManager) SetStatus(paneTarget, status string) error {
	return m.paneIO.SetUserVar(paneTarget, "status", status)
}

// SetCWD updates the @cwd tracking variable.
func (m *paneStateManager) SetCWD(paneTarget, cwd string) error {
	return m.paneIO.SetUserVar(paneTarget, "cwd", cwd)
}

// ResetCWD clears the @cwd tracking variable so the next delivery re-syncs.
func (m *paneStateManager) ResetCWD(paneTarget string) error {
	return m.paneIO.SetUserVar(paneTarget, "cwd", "")
}

// GetCWD returns the current tracked working directory.
func (m *paneStateManager) GetCWD(paneTarget string) string {
	v, _ := m.paneIO.GetUserVar(paneTarget, "cwd")
	return v
}

// AgentStateEvicted is the @agent_state value the daemon writes after
// respawning a worker pane to the project root for worktree cleanup.
// status.go honours this sentinel so `maestro status` does not flip the
// worker to "dead" while it sits in shell between cleanup and the next
// dispatch — the agent process is intentionally not running, not crashed.
const AgentStateEvicted = "evicted"

// SetAgentState writes the @agent_state user variable on the pane. The
// daemon uses it to signal "the lack of a live agent process here is
// intentional" so live-process probes (status.go) can distinguish a
// daemon-driven eviction from a crash. Empty state means "normal".
func (m *paneStateManager) SetAgentState(paneTarget, state string) error {
	return m.paneIO.SetUserVar(paneTarget, "agent_state", state)
}

// ResetAgentState clears @agent_state. Called after ensureClaudeRunning
// re-launches the agent so subsequent shell detections are once again
// real crash signals.
func (m *paneStateManager) ResetAgentState(paneTarget string) error {
	return m.paneIO.SetUserVar(paneTarget, "agent_state", "")
}

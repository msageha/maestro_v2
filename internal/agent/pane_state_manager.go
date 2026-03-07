package agent

import "fmt"

// PaneStateManager centralizes management of tmux user variables that track
// pane state (clear_ready, clear_ready_pid, cwd, status). This extracts the
// scattered Get/Set/Reset logic from Executor into a single responsible type.
//
// All state is stored as tmux user variables (@clear_ready, @clear_ready_pid,
// @cwd, @status) on the target pane. PaneStateManager is stateless itself —
// it delegates all reads/writes to PaneIO.
type PaneStateManager struct {
	paneIO PaneIO
}

// NewPaneStateManager creates a PaneStateManager backed by the given PaneIO.
func NewPaneStateManager(paneIO PaneIO) *PaneStateManager {
	return &PaneStateManager{paneIO: paneIO}
}

// IsClearReady returns whether the pane has an active conversation and is
// ready for /clear. Returns false if the variable is unset or on error.
func (m *PaneStateManager) IsClearReady(paneTarget string) bool {
	v, err := m.paneIO.GetUserVar(paneTarget, "clear_ready")
	return err == nil && v == "true"
}

// SetClearReady marks the pane as having an active conversation (clear_ready=true)
// and records the current PID for restart detection.
func (m *PaneStateManager) SetClearReady(paneTarget, pid string) error {
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
func (m *PaneStateManager) ResetClearReady(paneTarget string) error {
	var firstErr error
	if err := m.paneIO.SetUserVar(paneTarget, "clear_ready", ""); err != nil {
		firstErr = fmt.Errorf("reset clear_ready: %w", err)
	}
	if err := m.paneIO.SetUserVar(paneTarget, "clear_ready_pid", ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("reset clear_ready_pid: %w", err)
	}
	return firstErr
}

// DetectProcessRestart checks if the pane's process PID has changed since
// clear_ready_pid was stored. Returns true if a restart was detected and
// clear_ready was reset.
//
// To mitigate TOCTOU between GetPanePID and the comparison, the PID is
// re-read after resetting clear_ready. If it changed again, the caller
// still gets the latest PID.
func (m *PaneStateManager) DetectProcessRestart(paneTarget string) (restarted bool, currentPID string, err error) {
	currentPID, err = m.paneIO.GetPanePID(paneTarget)
	if err != nil {
		return false, "", fmt.Errorf("get pane pid: %w", err)
	}
	if currentPID == "" {
		return false, "", fmt.Errorf("get pane pid: empty PID returned")
	}

	storedPID, _ := m.paneIO.GetUserVar(paneTarget, "clear_ready_pid")
	if storedPID != "" && storedPID != currentPID {
		// Process restarted — reset clear_ready.
		// Reset errors are non-fatal: the caller should still know a restart
		// occurred even if the reset partially failed.
		resetErr := m.ResetClearReady(paneTarget)

		// Re-read PID to mitigate TOCTOU: if the process restarted again
		// between the first read and now, return the latest PID.
		if latestPID, pidErr := m.paneIO.GetPanePID(paneTarget); pidErr == nil && latestPID != "" {
			currentPID = latestPID
		}

		return true, currentPID, resetErr
	}

	return false, currentPID, nil
}

// SetStatus sets the @status user variable on the pane.
func (m *PaneStateManager) SetStatus(paneTarget, status string) error {
	return m.paneIO.SetUserVar(paneTarget, "status", status)
}

// SetCWD updates the @cwd tracking variable.
func (m *PaneStateManager) SetCWD(paneTarget, cwd string) error {
	return m.paneIO.SetUserVar(paneTarget, "cwd", cwd)
}

// GetCWD returns the current tracked working directory.
func (m *PaneStateManager) GetCWD(paneTarget string) string {
	v, _ := m.paneIO.GetUserVar(paneTarget, "cwd")
	return v
}

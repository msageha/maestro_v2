package tmux

import (
	"strings"
	"testing"
	"time"
)

// waitForCommand polls until the pane's current command matches expected.
// Best-effort e2e helper: logs a warning and proceeds on timeout rather than fataling.
// Uses ticker-based polling for proper resource cleanup.
func waitForCommand(t *testing.T, paneTarget, expected string, timeout time.Duration) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.Logf("WARNING: command %q not detected in pane %s, proceeding", expected, paneTarget)
			return
		case <-ticker.C:
			cmd, err := GetPaneCurrentCommand(paneTarget)
			if err == nil && strings.Contains(cmd, expected) {
				return
			}
		}
	}
}

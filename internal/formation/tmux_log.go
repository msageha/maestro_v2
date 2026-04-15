package formation

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/tmux"
)

// tmuxLogCloser holds the file handle for a tmux debug log so callers can
// defer cleanup via Close.
type tmuxLogCloser struct {
	file *os.File
}

// Close resets the tmux debug logger and closes the underlying file.
func (c *tmuxLogCloser) Close() error {
	tmux.SetDebugLogger(nil)
	if c.file != nil {
		return c.file.Close()
	}
	return nil
}

// initTmuxDebugLog creates (or opens) the tmux debug log file under
// maestroDir/logs/ and installs it as the tmux package debug logger.
// Returns a tmuxLogCloser whose Close method resets the logger and closes
// the file. If initialization fails, the logger is not set and a nil-safe
// closer is returned so callers can always defer Close().
func initTmuxDebugLog(maestroDir string) (*tmuxLogCloser, error) {
	logsDir := filepath.Join(maestroDir, "logs")
	if err := os.MkdirAll(logsDir, 0750); err != nil {
		return &tmuxLogCloser{}, fmt.Errorf("create tmux debug log directory: %w", err)
	}

	tmuxLogPath := filepath.Join(logsDir, "tmux_debug.log")
	f, err := os.OpenFile(tmuxLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // tmuxLogPath is constructed from a controlled application log directory
	if err != nil {
		return &tmuxLogCloser{}, fmt.Errorf("open tmux debug log: %w", err)
	}

	tmuxLogger := log.New(f, "", log.LstdFlags|log.Lmicroseconds)
	tmux.SetDebugLogger(tmuxLogger)
	return &tmuxLogCloser{file: f}, nil
}

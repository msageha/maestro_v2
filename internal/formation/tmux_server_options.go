package formation

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// serverOptionsBackupFile is the filename for saving tmux server options
// before hardening, so they can be restored during teardown.
const serverOptionsBackupFile = "server_options_backup.yaml"

// saveServerOptions queries the current tmux server option values and saves
// them to a backup file so they can be restored during teardown.
// If the tmux server is not running, tmux defaults are saved instead.
func saveServerOptions(maestroDir string) error {
	type optDef struct {
		name       string
		defaultVal string
	}
	opts := []optDef{
		{"exit-empty", "on"},       // tmux default
		{"exit-unattached", "off"}, // tmux default
	}

	saved := make(map[string]string, len(opts))
	for _, o := range opts {
		val, err := getServerOptionValue(o.name)
		if err == nil && val != "" {
			saved[o.name] = val
		} else {
			saved[o.name] = o.defaultVal
		}
	}

	backupPath := filepath.Join(maestroDir, serverOptionsBackupFile)
	return yamlutil.AtomicWrite(backupPath, saved)
}

// getServerOptionValue retrieves a tmux server-level option value.
// Returns an error if the tmux server is not running or the option doesn't exist.
func getServerOptionValue(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "show-options", "-s", "-v", name)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

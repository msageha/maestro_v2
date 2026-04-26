package daemon

import "path/filepath"

func verifySnapshotPath(maestroDir, commandID string) string {
	return filepath.Join(maestroDir, "state", "verify", commandID+".yaml")
}

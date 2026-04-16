package daemon

import "path/filepath"

// Path construction helpers centralise the directory layout of the .maestro
// directory so that callers do not scatter hardcoded string literals.

// commandQueuePath returns the path to the planner command queue file.
func commandQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "planner.yaml")
}

// taskQueuePath returns the path to a worker's task queue file.
func taskQueuePath(maestroDir, target string) string {
	return filepath.Join(maestroDir, "queue", target+".yaml")
}

// notificationQueuePath returns the path to the orchestrator notification queue file.
func notificationQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "orchestrator.yaml")
}

// signalQueuePath returns the path to the planner signal queue file.
func signalQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "planner_signals.yaml")
}

// commandStatePath returns the path to a command's state file.
func commandStatePath(maestroDir, commandID string) string {
	return filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
}

// queueDirPath returns the path to the queue directory.
func queueDirPath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue")
}

// resultsDirPath returns the path to the results directory.
func resultsDirPath(maestroDir string) string {
	return filepath.Join(maestroDir, "results")
}

// resultFilePath returns the path to a reporter's result file.
func resultFilePath(maestroDir, reporter string) string {
	return filepath.Join(maestroDir, "results", reporter+".yaml")
}

// quarantineDirPath returns the path to the quarantine directory.
func quarantineDirPath(maestroDir string) string {
	return filepath.Join(maestroDir, "quarantine")
}

// learningsFilePath returns the path to the learnings state file.
func learningsFilePath(maestroDir string) string {
	return filepath.Join(maestroDir, "state", "learnings.yaml")
}

// verifyConfigPath returns the path to the verify.yaml file.
func verifyConfigPath(maestroDir string) string {
	return filepath.Join(maestroDir, "verify.yaml")
}

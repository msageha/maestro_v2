package reconcile

import "path/filepath"

// Path construction helpers centralise the .maestro queue layout for the
// reconcile package (mirrors internal/daemon/paths.go; duplicated locally to
// keep the package decoupled from daemon, matching the repo's DIP style).

// queueDirPath returns the path to the queue directory.
func queueDirPath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue")
}

// taskQueuePath returns the path to a worker's task queue file.
func taskQueuePath(maestroDir, workerID string) string {
	return filepath.Join(maestroDir, "queue", workerID+".yaml")
}

// commandQueuePath returns the path to the planner command queue file.
func commandQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "planner.yaml")
}

// notificationQueuePath returns the path to the orchestrator notification queue file.
func notificationQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "orchestrator.yaml")
}

// signalQueuePath returns the path to the planner signal queue file.
func signalQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "planner_signals.yaml")
}

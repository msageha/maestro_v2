package daemon

import (
	"path/filepath"
	"testing"
)

func TestPathHelpers(t *testing.T) {
	maestroDir := "/project/.maestro"

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"commandQueuePath", commandQueuePath(maestroDir), filepath.Join(maestroDir, "queue", "planner.yaml")},
		{"taskQueuePath", taskQueuePath(maestroDir, "worker1"), filepath.Join(maestroDir, "queue", "worker1.yaml")},
		{"notificationQueuePath", notificationQueuePath(maestroDir), filepath.Join(maestroDir, "queue", "orchestrator.yaml")},
		{"signalQueuePath", signalQueuePath(maestroDir), filepath.Join(maestroDir, "queue", "planner_signals.yaml")},
		{"commandStatePath", commandStatePath(maestroDir, "cmd_001"), filepath.Join(maestroDir, "state", "commands", "cmd_001.yaml")},
		{"queueDirPath", queueDirPath(maestroDir), filepath.Join(maestroDir, "queue")},
		{"resultsDirPath", resultsDirPath(maestroDir), filepath.Join(maestroDir, "results")},
		{"resultFilePath", resultFilePath(maestroDir, "worker1"), filepath.Join(maestroDir, "results", "worker1.yaml")},
		{"quarantineDirPath", quarantineDirPath(maestroDir), filepath.Join(maestroDir, "quarantine")},
		{"learningsFilePath", learningsFilePath(maestroDir), filepath.Join(maestroDir, "state", "learnings.yaml")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

package plan

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// RebuildOptions holds the configuration for rebuilding command state from worker results.
type RebuildOptions struct {
	CommandID  string
	MaestroDir string
	LockMap    *lock.MutexMap
}

// Rebuild reconstructs the command state by scanning worker result files and applying the latest status for each task.
func Rebuild(opts RebuildOptions) error {
	if opts.LockMap == nil {
		return fmt.Errorf("LockMap is required")
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Defensive init: AppliedResultIDs may be nil in old/corrupted state files
	if state.AppliedResultIDs == nil {
		state.AppliedResultIDs = make(map[string]string)
	}

	// Scan all results/worker{N}.yaml files for tasks belonging to this command
	resultsDir := filepath.Join(opts.MaestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return fmt.Errorf("read results dir: %w", err)
	}

	// Collect the latest result per task_id across all worker files.
	// Results may span multiple files and file read order (os.ReadDir) does not
	// guarantee chronological ordering, so we compare CreatedAt timestamps.
	type latestResult struct {
		status    model.Status
		resultID  string
		createdAt time.Time
	}
	latestByTask := make(map[string]latestResult)

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			continue
		}

		for _, r := range rf.Results {
			if r.CommandID != opts.CommandID {
				continue
			}

			if _, ok := state.TaskStates[r.TaskID]; !ok {
				continue // unknown task
			}

			createdAt, err := time.Parse(time.RFC3339, r.CreatedAt)
			if err != nil {
				continue // skip results with unparseable timestamps
			}

			if existing, ok := latestByTask[r.TaskID]; ok {
				if createdAt.Before(existing.createdAt) {
					continue // older result, skip
				}
				if createdAt.Equal(existing.createdAt) && r.ID < existing.resultID {
					continue // same timestamp, use deterministic tie-break by ID
				}
			}

			latestByTask[r.TaskID] = latestResult{
				status:    r.Status,
				resultID:  r.ID,
				createdAt: createdAt,
			}
		}
	}

	// Apply the latest result for each task with transition validation.
	// Rebuild allows non-terminal → terminal (crash recovery: pending/in_progress → completed/failed)
	// but rejects terminal → any (prevents overwriting already-settled state).
	for taskID, lr := range latestByTask {
		currentStatus := state.TaskStates[taskID]
		if model.IsTerminal(currentStatus) {
			log.Printf("rebuild: skipping task %s: current status %q is terminal, cannot transition to %q",
				taskID, currentStatus, lr.status)
			continue
		}
		state.TaskStates[taskID] = lr.status
		state.AppliedResultIDs[taskID] = lr.resultID
	}

	now := time.Now().UTC().Format(time.RFC3339)
	state.LastReconciledAt = &now
	state.UpdatedAt = now

	return sm.SaveState(state)
}

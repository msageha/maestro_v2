package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

type RebuildOptions struct {
	CommandID  string
	MaestroDir string
	LockMap    *lock.MutexMap
}

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

	// Scan all results/worker{N}.yaml files for tasks belonging to this command
	resultsDir := filepath.Join(opts.MaestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return fmt.Errorf("read results dir: %w", err)
	}

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

			state.TaskStates[r.TaskID] = r.Status
			state.AppliedResultIDs[r.TaskID] = r.ID
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	state.LastReconciledAt = &now
	state.UpdatedAt = now

	return sm.SaveState(state)
}

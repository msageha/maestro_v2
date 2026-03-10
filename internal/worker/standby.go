// Package worker provides worker assignment and standby management.
package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// WorkerStatus represents the status summary for a single worker.
type WorkerStatus struct {
	WorkerID        string `json:"worker_id"`
	Model           string `json:"model"`
	PendingCount    int    `json:"pending_count"`
	InProgressCount int    `json:"in_progress_count"`
	Status          string `json:"status"` // "idle", "busy", or "error"
}

// StandbyOptions configures the standby scan.
type StandbyOptions struct {
	MaestroDir  string
	Config      model.Config
	ModelFilter string
}

// Standby scans all worker queue files and returns a JSON-serializable summary.
func Standby(opts StandbyOptions) ([]WorkerStatus, error) {
	queueDir := filepath.Join(opts.MaestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []WorkerStatus{}, nil
		}
		return nil, fmt.Errorf("read queue dir: %w", err)
	}

	var results []WorkerStatus

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		workerModel := resolveWorkerModel(workerID, opts.Config)

		if opts.ModelFilter != "" && workerModel != opts.ModelFilter {
			continue
		}

		path := filepath.Join(queueDir, name)
		maxBytes := opts.Config.Limits.MaxYAMLFileBytes
		if maxBytes <= 0 {
			maxBytes = model.DefaultMaxYAMLFileBytes
		}
		data, err := readQueueFile(path, maxBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", name, err)
			results = append(results, WorkerStatus{
				WorkerID: workerID,
				Model:    workerModel,
				Status:   "error",
			})
			continue
		}

		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot parse %s: %v\n", name, err)
			results = append(results, WorkerStatus{
				WorkerID: workerID,
				Model:    workerModel,
				Status:   "error",
			})
			continue
		}

		pending, inProgress := 0, 0
		for _, task := range tq.Tasks {
			switch task.Status {
			case model.StatusPending:
				pending++
			case model.StatusInProgress:
				inProgress++
			}
		}

		status := "idle"
		if pending > 0 || inProgress > 0 {
			status = "busy"
		}

		results = append(results, WorkerStatus{
			WorkerID:        workerID,
			Model:           workerModel,
			PendingCount:    pending,
			InProgressCount: inProgress,
			Status:          status,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].WorkerID < results[j].WorkerID
	})

	if results == nil {
		results = []WorkerStatus{}
	}

	return results, nil
}

// StandbyJSON runs Standby and returns the result as a JSON string.
func StandbyJSON(opts StandbyOptions) (string, error) {
	statuses, err := Standby(opts)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(statuses, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}
	return string(data), nil
}

// readQueueFile opens, validates, and reads a queue YAML file.
// Extracted from loop to allow proper defer-based resource cleanup.
func readQueueFile(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if fi.Size() > int64(maxBytes) {
		return nil, fmt.Errorf("file too large (%d bytes > %d max)", fi.Size(), maxBytes)
	}

	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("read data too large (%d bytes > %d max)", len(data), maxBytes)
	}

	return data, nil
}

// resolveWorkerModel determines the model for a given worker ID from config.
// When boost is enabled, all workers are promoted to opus.
func resolveWorkerModel(workerID string, cfg model.Config) string {
	if cfg.Agents.Workers.Boost {
		return "opus"
	}
	if cfg.Agents.Workers.Models != nil {
		if m, ok := cfg.Agents.Workers.Models[workerID]; ok {
			return m
		}
	}
	if cfg.Agents.Workers.DefaultModel != "" {
		return cfg.Agents.Workers.DefaultModel
	}
	return "sonnet"
}

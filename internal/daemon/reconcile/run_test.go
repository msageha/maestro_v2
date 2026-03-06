package reconcile

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// fixedClock returns a deterministic time for testing.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// newTestRun creates a Run with real deps rooted at the given maestroDir.
func newTestRun(t *testing.T, maestroDir string) *Run {
	t.Helper()
	dl := core.NewDaemonLoggerFromLegacy("test", log.New(io.Discard, "", 0), core.LogLevelError)
	return &Run{
		Deps: &Deps{
			MaestroDir: maestroDir,
			LockMap:    lock.NewMutexMap(),
			DL:         dl,
			Clock:      fixedClock{t: time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)},
		},
		dirCache: make(map[string][]os.DirEntry),
	}
}

// writeTaskQueue writes a TaskQueue YAML file.
func writeTaskQueue(t *testing.T, path string, tasks []model.Task) {
	t.Helper()
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks:         tasks,
	}
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("writeTaskQueue: %v", err)
	}
}

// readTaskQueue reads and parses a TaskQueue YAML file.
func readTaskQueue(t *testing.T, path string) model.TaskQueue {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readTaskQueue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("readTaskQueue unmarshal: %v", err)
	}
	return tq
}

// writeCommandResultFile writes a CommandResultFile YAML file.
func writeCommandResultFile(t *testing.T, path string, results []model.CommandResult) {
	t.Helper()
	rf := model.CommandResultFile{
		SchemaVersion: 1,
		FileType:      "command_result",
		Results:       results,
	}
	if err := yamlutil.AtomicWrite(path, rf); err != nil {
		t.Fatalf("writeCommandResultFile: %v", err)
	}
}

// readCommandResultFile reads and parses a CommandResultFile YAML file.
func readCommandResultFile(t *testing.T, path string) model.CommandResultFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readCommandResultFile: %v", err)
	}
	var rf model.CommandResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("readCommandResultFile unmarshal: %v", err)
	}
	return rf
}

func TestBatchRemoveTaskIDsFromQueues(t *testing.T) {
	tests := []struct {
		name       string
		setupFiles map[string][]model.Task // filename -> tasks
		taskIDs    []string
		wantFiles  map[string][]model.Task // filename -> remaining tasks (full struct check)
		wantErr    bool
	}{
		{
			name:    "empty task IDs returns nil immediately",
			taskIDs: nil,
			wantErr: false,
		},
		{
			name: "single task removed from single worker queue",
			setupFiles: map[string][]model.Task{
				"worker1.yaml": {
					{ID: "task1", CommandID: "cmd1", Purpose: "do X"},
					{ID: "task2", CommandID: "cmd1", Purpose: "do Y", Status: "pending"},
				},
			},
			taskIDs: []string{"task1"},
			wantFiles: map[string][]model.Task{
				"worker1.yaml": {{ID: "task2", CommandID: "cmd1", Purpose: "do Y", Status: "pending"}},
			},
		},
		{
			name: "multiple tasks removed across multiple worker queues",
			setupFiles: map[string][]model.Task{
				"worker1.yaml": {
					{ID: "task1", CommandID: "cmd1"},
					{ID: "task2", CommandID: "cmd1", Purpose: "survive"},
				},
				"worker2.yaml": {
					{ID: "task3", CommandID: "cmd1"},
					{ID: "task4", CommandID: "cmd2", Purpose: "also survive"},
				},
			},
			taskIDs: []string{"task1", "task3"},
			wantFiles: map[string][]model.Task{
				"worker1.yaml": {{ID: "task2", CommandID: "cmd1", Purpose: "survive"}},
				"worker2.yaml": {{ID: "task4", CommandID: "cmd2", Purpose: "also survive"}},
			},
		},
		{
			name: "task ID not present in any queue is no-op",
			setupFiles: map[string][]model.Task{
				"worker1.yaml": {
					{ID: "task1", CommandID: "cmd1", Purpose: "unchanged"},
				},
			},
			taskIDs: []string{"nonexistent"},
			wantFiles: map[string][]model.Task{
				"worker1.yaml": {{ID: "task1", CommandID: "cmd1", Purpose: "unchanged"}},
			},
		},
		{
			name:    "queue dir does not exist returns nil",
			taskIDs: []string{"task1"},
			wantErr: false,
		},
		{
			name: "duplicate task IDs in input are handled correctly",
			setupFiles: map[string][]model.Task{
				"worker1.yaml": {
					{ID: "task1", CommandID: "cmd1"},
					{ID: "task2", CommandID: "cmd1", Purpose: "keep"},
				},
			},
			taskIDs: []string{"task1", "task1"},
			wantFiles: map[string][]model.Task{
				"worker1.yaml": {{ID: "task2", CommandID: "cmd1", Purpose: "keep"}},
			},
		},
		{
			name: "all tasks removed leaves empty task list",
			setupFiles: map[string][]model.Task{
				"worker1.yaml": {
					{ID: "task1", CommandID: "cmd1"},
				},
			},
			taskIDs: []string{"task1"},
			wantFiles: map[string][]model.Task{
				"worker1.yaml": {},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maestroDir := t.TempDir()
			run := newTestRun(t, maestroDir)

			if tt.setupFiles != nil {
				queueDir := filepath.Join(maestroDir, "queue")
				if err := os.MkdirAll(queueDir, 0755); err != nil {
					t.Fatalf("mkdir queue: %v", err)
				}
				for filename, tasks := range tt.setupFiles {
					writeTaskQueue(t, filepath.Join(queueDir, filename), tasks)
				}
			}

			err := run.BatchRemoveTaskIDsFromQueues(tt.taskIDs)

			if (err != nil) != tt.wantErr {
				t.Fatalf("BatchRemoveTaskIDsFromQueues() error = %v, wantErr = %v", err, tt.wantErr)
			}

			for filename, wantTasks := range tt.wantFiles {
				queuePath := filepath.Join(maestroDir, "queue", filename)
				got := readTaskQueue(t, queuePath)
				if len(got.Tasks) != len(wantTasks) {
					t.Errorf("%s: got %d tasks, want %d", filename, len(got.Tasks), len(wantTasks))
					continue
				}
				for i, want := range wantTasks {
					g := got.Tasks[i]
					if g.ID != want.ID || g.CommandID != want.CommandID || g.Purpose != want.Purpose || g.Status != want.Status {
						t.Errorf("%s: task[%d] = {ID:%q CommandID:%q Purpose:%q Status:%q}, want {ID:%q CommandID:%q Purpose:%q Status:%q}",
							filename, i, g.ID, g.CommandID, g.Purpose, g.Status, want.ID, want.CommandID, want.Purpose, want.Status)
					}
				}
				// Verify metadata fields are preserved
				if got.SchemaVersion != 1 {
					t.Errorf("%s: SchemaVersion = %d, want 1", filename, got.SchemaVersion)
				}
				if got.FileType != "task_queue" {
					t.Errorf("%s: FileType = %q, want %q", filename, got.FileType, "task_queue")
				}
			}
		})
	}
}

func TestBatchRemoveTaskIDsFromQueues_NonWorkerFilesSkipped(t *testing.T) {
	maestroDir := t.TempDir()
	run := newTestRun(t, maestroDir)

	queueDir := filepath.Join(maestroDir, "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plannerContent := []byte("should_not_be_touched: true\n")
	plannerPath := filepath.Join(queueDir, "planner.yaml")
	if err := os.WriteFile(plannerPath, plannerContent, 0644); err != nil {
		t.Fatalf("write planner.yaml: %v", err)
	}
	writeTaskQueue(t, filepath.Join(queueDir, "worker1.yaml"), []model.Task{
		{ID: "task1", CommandID: "cmd1"},
		{ID: "task2", CommandID: "cmd1"},
	})

	if err := run.BatchRemoveTaskIDsFromQueues([]string{"task1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner.yaml: %v", err)
	}
	if string(data) != string(plannerContent) {
		t.Errorf("planner.yaml was modified: %s", data)
	}
}

func TestBatchRemoveTaskIDsFromQueues_MalformedYAML(t *testing.T) {
	maestroDir := t.TempDir()
	run := newTestRun(t, maestroDir)

	queueDir := filepath.Join(maestroDir, "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write malformed YAML — the function logs a warning and continues (no error returned)
	if err := os.WriteFile(filepath.Join(queueDir, "worker1.yaml"), []byte(":::bad yaml"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Write a valid queue to confirm it's still processed
	writeTaskQueue(t, filepath.Join(queueDir, "worker2.yaml"), []model.Task{
		{ID: "task1", CommandID: "cmd1"},
		{ID: "task2", CommandID: "cmd1"},
	})

	err := run.BatchRemoveTaskIDsFromQueues([]string{"task1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// worker2 should still have task1 removed
	got := readTaskQueue(t, filepath.Join(queueDir, "worker2.yaml"))
	if len(got.Tasks) != 1 || got.Tasks[0].ID != "task2" {
		t.Errorf("worker2.yaml: unexpected tasks: %+v", got.Tasks)
	}
}

func TestQuarantineCommandResult(t *testing.T) {
	tests := []struct {
		name            string
		existingResults []model.CommandResult
		targetResult    model.CommandResult
		wantRemaining   []model.CommandResult // full struct check for surviving records
		wantQuarantined bool
		wantErr         bool
	}{
		{
			name: "normal: result moved to quarantine",
			existingResults: []model.CommandResult{
				{ID: "res1", CommandID: "cmd1", Status: "completed", Summary: "done", CreatedAt: "2026-01-01T00:00:00Z"},
				{ID: "res2", CommandID: "cmd1", Status: "completed", Summary: "also done", CreatedAt: "2026-01-02T00:00:00Z"},
			},
			targetResult: model.CommandResult{ID: "res1", CommandID: "cmd1"},
			wantRemaining: []model.CommandResult{
				{ID: "res2", CommandID: "cmd1", Status: "completed", Summary: "also done", CreatedAt: "2026-01-02T00:00:00Z"},
			},
			wantQuarantined: true,
		},
		{
			name: "result not found is no-op",
			existingResults: []model.CommandResult{
				{ID: "res1", CommandID: "cmd1", Status: "completed", Summary: "untouched"},
			},
			targetResult: model.CommandResult{ID: "nonexistent", CommandID: "cmd1"},
			wantRemaining: []model.CommandResult{
				{ID: "res1", CommandID: "cmd1", Status: "completed", Summary: "untouched"},
			},
			wantQuarantined: false,
		},
		{
			name:            "empty result file is no-op",
			existingResults: []model.CommandResult{},
			targetResult:    model.CommandResult{ID: "res1", CommandID: "cmd1"},
			wantRemaining:   []model.CommandResult{},
			wantQuarantined: false,
		},
		{
			name: "multiple results, only target quarantined",
			existingResults: []model.CommandResult{
				{ID: "res1", CommandID: "cmd1", Status: "completed", Summary: "first"},
				{ID: "res2", CommandID: "cmd1", Status: "failed", Summary: "bad one"},
				{ID: "res3", CommandID: "cmd2", Status: "completed", Summary: "third"},
			},
			targetResult: model.CommandResult{ID: "res2", CommandID: "cmd1"},
			wantRemaining: []model.CommandResult{
				{ID: "res1", CommandID: "cmd1", Status: "completed", Summary: "first"},
				{ID: "res3", CommandID: "cmd2", Status: "completed", Summary: "third"},
			},
			wantQuarantined: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maestroDir := t.TempDir()
			run := newTestRun(t, maestroDir)

			resultsDir := filepath.Join(maestroDir, "results")
			if err := os.MkdirAll(resultsDir, 0755); err != nil {
				t.Fatalf("mkdir results: %v", err)
			}
			resultPath := filepath.Join(resultsDir, "planner.yaml")
			writeCommandResultFile(t, resultPath, tt.existingResults)

			err := run.QuarantineCommandResult(resultPath, tt.targetResult)

			if (err != nil) != tt.wantErr {
				t.Fatalf("QuarantineCommandResult() error = %v, wantErr = %v", err, tt.wantErr)
			}

			// Verify remaining results with full struct comparison
			got := readCommandResultFile(t, resultPath)
			if len(got.Results) != len(tt.wantRemaining) {
				t.Fatalf("remaining results: got %d, want %d", len(got.Results), len(tt.wantRemaining))
			}
			for i, want := range tt.wantRemaining {
				g := got.Results[i]
				if g.ID != want.ID || g.CommandID != want.CommandID || g.Status != want.Status || g.Summary != want.Summary || g.CreatedAt != want.CreatedAt {
					t.Errorf("result[%d] mismatch:\n  got:  {ID:%q CommandID:%q Status:%q Summary:%q CreatedAt:%q}\n  want: {ID:%q CommandID:%q Status:%q Summary:%q CreatedAt:%q}",
						i, g.ID, g.CommandID, g.Status, g.Summary, g.CreatedAt, want.ID, want.CommandID, want.Status, want.Summary, want.CreatedAt)
				}
			}
			// Verify file metadata preserved
			if got.SchemaVersion != 1 {
				t.Errorf("SchemaVersion = %d, want 1", got.SchemaVersion)
			}
			if got.FileType != "command_result" {
				t.Errorf("FileType = %q, want %q", got.FileType, "command_result")
			}

			// Verify quarantine file
			quarantineDir := filepath.Join(maestroDir, "quarantine")
			entries, readErr := os.ReadDir(quarantineDir)
			if tt.wantQuarantined {
				if readErr != nil {
					t.Fatalf("read quarantine dir: %v", readErr)
				}
				if len(entries) != 1 {
					t.Fatalf("expected 1 quarantine file, got %d", len(entries))
				}
				qPath := filepath.Join(quarantineDir, entries[0].Name())
				data, err := os.ReadFile(qPath)
				if err != nil {
					t.Fatalf("read quarantine file: %v", err)
				}
				var quarantined model.CommandResult
				if err := yamlv3.Unmarshal(data, &quarantined); err != nil {
					t.Fatalf("unmarshal quarantine: %v", err)
				}
				if quarantined.ID != tt.targetResult.ID {
					t.Errorf("quarantined ID = %q, want %q", quarantined.ID, tt.targetResult.ID)
				}
				// Verify the quarantine file has data from disk snapshot, not the passed struct
				for _, orig := range tt.existingResults {
					if orig.ID == tt.targetResult.ID {
						if quarantined.Summary != orig.Summary {
							t.Errorf("quarantined Summary = %q, want %q (from disk)", quarantined.Summary, orig.Summary)
						}
						if quarantined.Status != orig.Status {
							t.Errorf("quarantined Status = %q, want %q (from disk)", quarantined.Status, orig.Status)
						}
						break
					}
				}
			} else {
				// Quarantine dir may or may not exist, but should have no files
				if readErr == nil && len(entries) > 0 {
					t.Errorf("expected no quarantine files, got %d", len(entries))
				}
			}
		})
	}
}

func TestQuarantineCommandResult_MissingResultFile(t *testing.T) {
	maestroDir := t.TempDir()
	run := newTestRun(t, maestroDir)

	// results dir exists but planner.yaml does not — LoadCommandResultFile returns empty struct
	resultsDir := filepath.Join(maestroDir, "results")
	if err := os.MkdirAll(resultsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	resultPath := filepath.Join(resultsDir, "planner.yaml")

	err := run.QuarantineCommandResult(resultPath, model.CommandResult{ID: "res1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Quarantine dir is created (MkdirAll runs before the no-op check) but should have no files
	entries, err := os.ReadDir(filepath.Join(maestroDir, "quarantine"))
	if err != nil {
		t.Fatalf("read quarantine dir: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("expected no quarantine files, got %d", len(entries))
	}
}

func TestQuarantineCommandResult_MalformedResultFile(t *testing.T) {
	maestroDir := t.TempDir()
	run := newTestRun(t, maestroDir)

	resultsDir := filepath.Join(maestroDir, "results")
	if err := os.MkdirAll(resultsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	resultPath := filepath.Join(resultsDir, "planner.yaml")
	if err := os.WriteFile(resultPath, []byte(":::bad yaml"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := run.QuarantineCommandResult(resultPath, model.CommandResult{ID: "res1"})
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

func TestExtractWorkerID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"worker1.yaml", "worker1"},
		{"worker99.yaml", "worker99"},
		{"planner.yaml", ""},
		{"worker.yaml", "worker"},
		{"notworker1.yaml", ""},
		{"worker1.json", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ExtractWorkerID(tt.input)
			if got != tt.want {
				t.Errorf("ExtractWorkerID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRemoveFromSlice(t *testing.T) {
	tests := []struct {
		name   string
		s      []string
		target string
		want   []string
	}{
		{"remove existing", []string{"a", "b", "c"}, "b", []string{"a", "c"}},
		{"remove nonexistent", []string{"a", "b"}, "z", []string{"a", "b"}},
		{"remove from empty", nil, "a", nil},
		{"remove duplicates", []string{"a", "b", "a"}, "a", []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RemoveFromSlice(tt.s, tt.target)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

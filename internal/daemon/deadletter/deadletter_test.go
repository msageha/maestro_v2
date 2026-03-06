package deadletter

import (
	"bytes"
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

func setupTestMaestroDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	for _, sub := range []string{"queue", "results", "logs"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
	return maestroDir
}

func newTestProcessor(maestroDir string, cfg model.Config) *Processor {
	return NewProcessor(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
}

func TestProcessorCommandDeadLetter(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{CommandDispatch: 3},
	}
	p := newTestProcessor(maestroDir, cfg)

	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatal(err)
	}

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_001",
				Status:    model.StatusPending,
				Attempts:  3,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_002",
				Status:    model.StatusPending,
				Attempts:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_003",
				Status:    model.StatusInProgress,
				Attempts:  5,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := p.ProcessCommandDeadLetters(&cq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}
	if results[0].EntryID != "cmd_001" {
		t.Errorf("expected cmd_001, got %s", results[0].EntryID)
	}
	if results[0].QueueType != "planner" {
		t.Errorf("expected planner queue type, got %s", results[0].QueueType)
	}
	if !dirty {
		t.Error("dirty should be true")
	}
	if len(cq.Commands) != 2 {
		t.Fatalf("expected 2 commands remaining, got %d", len(cq.Commands))
	}

	// Verify archive file was created
	archiveDir := filepath.Join(maestroDir, "dead_letters")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 archive file, got %d", len(entries))
	}
}

func TestProcessorCommandDeadLetterWithState(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{CommandDispatch: 2},
	}
	p := newTestProcessor(maestroDir, cfg)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     "cmd_001",
		PlanStatus:    model.PlanStatusSealed,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_001.yaml"), &state); err != nil {
		t.Fatal(err)
	}

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_001",
				Status:    model.StatusPending,
				Attempts:  2,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := p.ProcessCommandDeadLetters(&cq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "cmd_001.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedState model.CommandState
	if err := yamlv3.Unmarshal(data, &updatedState); err != nil {
		t.Fatal(err)
	}
	if updatedState.PlanStatus != model.PlanStatusFailed {
		t.Errorf("expected plan_status=failed, got %s", updatedState.PlanStatus)
	}

	pending := p.DrainPendingNotifications()
	if len(pending) != 1 {
		t.Fatalf("expected 1 buffered notification, got %d", len(pending))
	}
	if pending[0].Type != "command_failed" {
		t.Errorf("expected command_failed type, got %s", pending[0].Type)
	}
}

func TestProcessorTaskDeadLetter(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{TaskDispatch: 5},
	}
	p := newTestProcessor(maestroDir, cfg)

	for _, sub := range []string{"state/commands", "results"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}

	tq := model.TaskQueue{
		Tasks: []model.Task{
			{
				ID:        "task_001",
				CommandID: "cmd_001",
				Status:    model.StatusPending,
				Attempts:  5,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "task_002",
				CommandID: "cmd_001",
				Status:    model.StatusPending,
				Attempts:  2,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := p.ProcessTaskDeadLetters("worker1", &tq, filepath.Join(maestroDir, "queue", "worker1.yaml"), &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}
	if results[0].EntryID != "task_001" {
		t.Errorf("expected task_001, got %s", results[0].EntryID)
	}
	if results[0].QueueType != "worker1" {
		t.Errorf("expected worker1 queue type, got %s", results[0].QueueType)
	}
	if !dirty {
		t.Error("dirty should be true")
	}
	if len(tq.Tasks) != 1 {
		t.Fatalf("expected 1 task remaining, got %d", len(tq.Tasks))
	}

	// Verify synthetic result was written
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatal(err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 synthetic result, got %d", len(rf.Results))
	}
	if rf.Results[0].Status != model.StatusFailed {
		t.Errorf("expected failed status, got %s", rf.Results[0].Status)
	}
}

func TestProcessorNotificationDeadLetter(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{OrchestratorNotificationDispatch: 3},
	}
	p := newTestProcessor(maestroDir, cfg)

	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{
				ID:        "ntf_001",
				CommandID: "cmd_001",
				Type:      "command_completed",
				Status:    model.StatusPending,
				Attempts:  3,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "ntf_002",
				CommandID: "cmd_002",
				Type:      "command_completed",
				Status:    model.StatusPending,
				Attempts:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := p.ProcessNotificationDeadLetters(&nq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}
	if results[0].EntryID != "ntf_001" {
		t.Errorf("expected ntf_001, got %s", results[0].EntryID)
	}
	if !dirty {
		t.Error("dirty should be true")
	}
	if len(nq.Notifications) != 1 {
		t.Fatalf("expected 1 notification remaining, got %d", len(nq.Notifications))
	}
}

func TestProcessorMaxAttemptsZero(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{
			CommandDispatch:                  0,
			TaskDispatch:                     0,
			OrchestratorNotificationDispatch: 0,
		},
	}
	p := newTestProcessor(maestroDir, cfg)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_001", Status: model.StatusPending, Attempts: 999},
		},
	}
	dirty := false
	results := p.ProcessCommandDeadLetters(&cq, &dirty)
	if len(results) != 0 {
		t.Errorf("expected 0 dead letters with maxAttempts=0, got %d", len(results))
	}
}

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestDaemonWithLearnings(t *testing.T) *Daemon {
	t.Helper()
	d := newTestDaemon(t)
	d.config.Learnings = model.LearningsConfig{
		Enabled:          true,
		MaxEntries:       100,
		MaxContentLength: 500,
	}
	return d
}

func readLearningsFile(t *testing.T, d *Daemon) model.LearningsFile {
	t.Helper()
	path := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read learnings file: %v", err)
	}
	var lf model.LearningsFile
	if err := yamlv3.Unmarshal(data, &lf); err != nil {
		t.Fatalf("parse learnings file: %v", err)
	}
	return lf
}

func TestLearnings_BasicWrite(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"learned thing 1", "learned thing 2"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 2 {
		t.Fatalf("expected 2 learnings, got %d", len(lf.Learnings))
	}
	if lf.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", lf.SchemaVersion)
	}
	if lf.FileType != "state_learnings" {
		t.Errorf("file_type = %q, want %q", lf.FileType, "state_learnings")
	}
	if lf.Learnings[0].Content != "learned thing 1" {
		t.Errorf("content[0] = %q, want %q", lf.Learnings[0].Content, "learned thing 1")
	}
	if lf.Learnings[1].Content != "learned thing 2" {
		t.Errorf("content[1] = %q, want %q", lf.Learnings[1].Content, "learned thing 2")
	}
	if lf.Learnings[0].CommandID != commandID {
		t.Errorf("command_id = %q, want %q", lf.Learnings[0].CommandID, commandID)
	}
	if lf.Learnings[0].ResultID == "" {
		t.Error("expected non-empty result_id")
	}
}

func TestLearnings_Disabled(t *testing.T) {
	d := newTestDaemon(t)
	// Learnings disabled by default
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"should not be written"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	path := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("learnings file should not exist when feature is disabled")
	}
}

func TestLearnings_DeduplicationByResultAndContent(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"learning A"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("first write failed: %v", resp.Error)
	}

	var result1 map[string]string
	json.Unmarshal(resp.Data, &result1)
	resultID := result1["result_id"]

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 1 {
		t.Fatalf("expected 1 learning after first write, got %d", len(lf.Learnings))
	}

	// Simulate idempotent retry: call writeLearnings again with same resultID + content
	err := d.writeLearnings(ResultWriteParams{
		CommandID: commandID,
		Learnings: []string{"learning A"},
	}, resultID)
	if err != nil {
		t.Fatalf("writeLearnings retry: %v", err)
	}

	lf2 := readLearningsFile(t, d)
	if len(lf2.Learnings) != 1 {
		t.Fatalf("expected 1 learning after dedup retry, got %d", len(lf2.Learnings))
	}

	// Different content with same resultID should be added
	err = d.writeLearnings(ResultWriteParams{
		CommandID: commandID,
		Learnings: []string{"learning B"},
	}, resultID)
	if err != nil {
		t.Fatalf("writeLearnings different content: %v", err)
	}

	lf3 := readLearningsFile(t, d)
	if len(lf3.Learnings) != 2 {
		t.Fatalf("expected 2 learnings after different content, got %d", len(lf3.Learnings))
	}
}

func TestLearnings_ContentTruncation(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	d.config.Learnings.MaxContentLength = 10

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	longContent := "this is a very long learning content that should be truncated"
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{longContent},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 1 {
		t.Fatalf("expected 1 learning, got %d", len(lf.Learnings))
	}
	if len([]rune(lf.Learnings[0].Content)) != 10 {
		t.Errorf("content rune length = %d, want 10", len([]rune(lf.Learnings[0].Content)))
	}
	if lf.Learnings[0].Content != "this is a " {
		t.Errorf("truncated content = %q, want %q", lf.Learnings[0].Content, "this is a ")
	}
}

func TestLearnings_ContentTruncation_UTF8(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	d.config.Learnings.MaxContentLength = 5

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Each Japanese character is one rune but multi-byte in UTF-8
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"あいうえおかきくけこ"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	lf := readLearningsFile(t, d)
	if lf.Learnings[0].Content != "あいうえお" {
		t.Errorf("content = %q, want %q", lf.Learnings[0].Content, "あいうえお")
	}
}

func TestLearnings_MaxEntriesFIFO(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	d.config.Learnings.MaxEntries = 3

	// Pre-populate with 2 existing learnings
	lf := model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings: []model.Learning{
			{ResultID: "res_0000000001_old00001", CommandID: "cmd_0000000001_old00001", Content: "old 1", CreatedAt: "2026-01-01T00:00:00Z"},
			{ResultID: "res_0000000001_old00002", CommandID: "cmd_0000000001_old00002", Content: "old 2", CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	if err := yamlutil.AtomicWrite(learningsPath, lf); err != nil {
		t.Fatalf("write initial learnings: %v", err)
	}

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Add 2 more learnings (total would be 4, exceeds max of 3)
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"new 1", "new 2"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	result := readLearningsFile(t, d)
	if len(result.Learnings) != 3 {
		t.Fatalf("expected 3 learnings (max), got %d", len(result.Learnings))
	}

	// Oldest entry ("old 1") should be evicted, keeping "old 2", "new 1", "new 2"
	if result.Learnings[0].Content != "old 2" {
		t.Errorf("learnings[0].content = %q, want %q", result.Learnings[0].Content, "old 2")
	}
	if result.Learnings[1].Content != "new 1" {
		t.Errorf("learnings[1].content = %q, want %q", result.Learnings[1].Content, "new 1")
	}
	if result.Learnings[2].Content != "new 2" {
		t.Errorf("learnings[2].content = %q, want %q", result.Learnings[2].Content, "new 2")
	}
}

func TestLearnings_EmptyContentSkipped(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"", "valid", ""},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 1 {
		t.Fatalf("expected 1 learning (empty skipped), got %d", len(lf.Learnings))
	}
	if lf.Learnings[0].Content != "valid" {
		t.Errorf("content = %q, want %q", lf.Learnings[0].Content, "valid")
	}
}

func TestLearnings_WriteFailureDoesNotFailResultWrite(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Make state dir read-only to force learnings write failure
	stateDir := filepath.Join(d.maestroDir, "state")
	os.Chmod(stateDir, 0555)
	defer os.Chmod(stateDir, 0755)

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"should fail silently"},
	})

	resp := d.handleResultWrite(req)
	// Result write should still succeed even if learnings write fails
	if !resp.Success {
		t.Fatalf("expected success despite learnings write failure, got error: %v", resp.Error)
	}

	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	if result["result_id"] == "" {
		t.Error("expected non-empty result_id")
	}
}

func TestLearnings_StartupValidation_CorruptFile(t *testing.T) {
	d := newTestDaemonWithLearnings(t)

	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")

	// Create quarantine directory (needed for recovery)
	os.MkdirAll(filepath.Join(d.maestroDir, "quarantine"), 0755)

	// Write corrupt content
	os.WriteFile(learningsPath, []byte("not: valid: yaml: [[["), 0644)

	// Validate should recover without panicking
	d.validateLearningsFile()

	// File should now be a valid skeleton or recovered
	data, err := os.ReadFile(learningsPath)
	if err != nil {
		t.Fatalf("read learnings after recovery: %v", err)
	}
	var lf model.LearningsFile
	if err := yamlv3.Unmarshal(data, &lf); err != nil {
		t.Fatalf("recovered file is not valid YAML: %v", err)
	}
	if lf.FileType != "state_learnings" {
		t.Errorf("file_type = %q, want %q", lf.FileType, "state_learnings")
	}
}

func TestLearnings_StartupValidation_NoFile(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	// No learnings file — should be a no-op, no panic
	d.validateLearningsFile()
}

func TestLearnings_StartupValidation_ValidFile(t *testing.T) {
	d := newTestDaemonWithLearnings(t)

	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	lf := model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings: []model.Learning{
			{ResultID: "res_0000000001_existing", Content: "existing", CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(learningsPath, lf)

	// Should not modify valid file
	d.validateLearningsFile()

	result := readLearningsFile(t, d)
	if len(result.Learnings) != 1 {
		t.Errorf("expected 1 learning after validate, got %d", len(result.Learnings))
	}
}

func TestLearnings_NoLearningsParam(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// No learnings field — should work fine
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// No learnings file should be created
	path := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("learnings file should not exist when no learnings provided")
	}
}

func TestLearnings_MultipleLearningsSameContent(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Duplicate content in same request should be deduped
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"same content", "same content", "different content"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 2 {
		t.Fatalf("expected 2 learnings (deduped), got %d", len(lf.Learnings))
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"あいう", 2, "あい"},
		{"", 5, ""},
		{"abc", 0, ""},
	}
	for _, tt := range tests {
		got := truncateRunes(tt.input, tt.max)
		if got != tt.expected {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
		}
	}
}

func TestLearningsConfig_Defaults(t *testing.T) {
	cfg := model.LearningsConfig{}
	if cfg.EffectiveMaxEntries() != 100 {
		t.Errorf("EffectiveMaxEntries() = %d, want 100", cfg.EffectiveMaxEntries())
	}
	if cfg.EffectiveMaxContentLength() != 500 {
		t.Errorf("EffectiveMaxContentLength() = %d, want 500", cfg.EffectiveMaxContentLength())
	}

	cfg2 := model.LearningsConfig{MaxEntries: 50, MaxContentLength: 200}
	if cfg2.EffectiveMaxEntries() != 50 {
		t.Errorf("EffectiveMaxEntries() = %d, want 50", cfg2.EffectiveMaxEntries())
	}
	if cfg2.EffectiveMaxContentLength() != 200 {
		t.Errorf("EffectiveMaxContentLength() = %d, want 200", cfg2.EffectiveMaxContentLength())
	}
}

func TestLearnings_RecoveryPreservesBackupEntries(t *testing.T) {
	d := newTestDaemonWithLearnings(t)

	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")

	// Create the .bak file manually with valid content that should be preserved
	bakLf := model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings: []model.Learning{
			{ResultID: "res_0000000001_backup01", CommandID: "cmd_0000000001_backup01", Content: "from backup", CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	if err := yamlutil.AtomicWrite(learningsPath+".bak", bakLf); err != nil {
		t.Fatalf("write .bak file: %v", err)
	}

	// Write corrupt content to the primary file
	os.WriteFile(learningsPath, []byte("corrupt: [[[invalid"), 0644)

	// Create quarantine dir
	os.MkdirAll(filepath.Join(d.maestroDir, "quarantine"), 0755)

	// writeLearnings should recover from backup and preserve old entries
	err := d.writeLearnings(ResultWriteParams{
		CommandID: "cmd_0000000001_new00001",
		Learnings: []string{"new learning"},
	}, "res_0000000001_new00001")
	if err != nil {
		t.Fatalf("writeLearnings with corrupt file: %v", err)
	}

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 2 {
		t.Fatalf("expected 2 learnings (1 from backup + 1 new), got %d", len(lf.Learnings))
	}
	if lf.Learnings[0].Content != "from backup" {
		t.Errorf("learnings[0].content = %q, want %q", lf.Learnings[0].Content, "from backup")
	}
	if lf.Learnings[1].Content != "new learning" {
		t.Errorf("learnings[1].content = %q, want %q", lf.Learnings[1].Content, "new learning")
	}
}

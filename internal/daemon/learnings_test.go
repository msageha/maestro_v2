package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/learnings"
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

// --- Integration Tests: E2E flow covering sanitization → SourceWorker → ReadTopK → Format ---

// TestLearningsIntegration_E2E_SanitizeSourceWorkerReadFormat exercises the full
// write → sanitize (truncation) → SourceWorker assignment → ReadTopKLearnings → FormatLearningsSection pipeline.
func TestLearningsIntegration_E2E_SanitizeSourceWorkerReadFormat(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	d.config.Learnings.MaxContentLength = 15
	d.config.Learnings.InjectCount = 5
	d.config.Learnings.TTLHours = 72

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker3"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Submit a result with learnings that exceed MaxContentLength
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"this is a very long learning that should be truncated to 15 runes", "short ok"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify stored data: sanitization + SourceWorker
	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 2 {
		t.Fatalf("expected 2 learnings, got %d", len(lf.Learnings))
	}
	if lf.Learnings[0].Content != "this is a very " {
		t.Errorf("truncation failed: content = %q, want %q", lf.Learnings[0].Content, "this is a very ")
	}
	if lf.Learnings[0].SourceWorker != workerID {
		t.Errorf("SourceWorker = %q, want %q", lf.Learnings[0].SourceWorker, workerID)
	}
	if lf.Learnings[1].Content != "short ok" {
		t.Errorf("content[1] = %q, want %q", lf.Learnings[1].Content, "short ok")
	}
	if lf.Learnings[1].SourceWorker != workerID {
		t.Errorf("SourceWorker[1] = %q, want %q", lf.Learnings[1].SourceWorker, workerID)
	}

	// ReadTopKLearnings → FormatLearningsSection
	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	readBack, err := learnings.ReadTopKLearnings(d.maestroDir, cfg, time.Now())
	if err != nil {
		t.Fatalf("ReadTopKLearnings: %v", err)
	}
	if len(readBack) != 2 {
		t.Fatalf("ReadTopKLearnings returned %d, want 2", len(readBack))
	}

	formatted := learnings.FormatLearningsSection(readBack)
	if !strings.Contains(formatted, "[from:worker3]") {
		t.Errorf("formatted output missing SourceWorker provenance: %q", formatted)
	}
	if !strings.Contains(formatted, "this is a very ") {
		t.Errorf("formatted output missing truncated content: %q", formatted)
	}
	if !strings.Contains(formatted, "short ok") {
		t.Errorf("formatted output missing second learning: %q", formatted)
	}
}

// TestLearningsIntegration_Dedup_TruncationCollision tests that two different long strings
// which truncate to the same prefix under one resultID are correctly deduplicated.
func TestLearningsIntegration_Dedup_TruncationCollision(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	d.config.Learnings.MaxContentLength = 5

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Both strings truncate to "AAAAA" (same resultID + same truncated content → dedup)
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"AAAAAXXX", "AAAAAYYY"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 1 {
		t.Fatalf("expected 1 learning after truncation-collision dedup, got %d", len(lf.Learnings))
	}
	if lf.Learnings[0].Content != "AAAAA" {
		t.Errorf("content = %q, want %q", lf.Learnings[0].Content, "AAAAA")
	}
}

// TestLearningsIntegration_TTL_EndToEnd writes learnings via handleResultWrite, then reads
// them back via ReadTopKLearnings with a TTL that excludes entries by timestamp.
func TestLearningsIntegration_TTL_EndToEnd(t *testing.T) {
	d := newTestDaemonWithLearnings(t)

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Write fresh learnings via handleResultWrite
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
		Learnings:  []string{"fresh learning"},
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Prepend an old entry directly (simulate an entry created long ago)
	lf := readLearningsFile(t, d)
	oldEntry := model.Learning{
		ResultID:     "res_0000000001_oldentry",
		CommandID:    commandID,
		Content:      "old learning",
		CreatedAt:    time.Now().UTC().Add(-200 * time.Hour).Format(time.RFC3339),
		SourceWorker: "worker2",
	}
	lf.Learnings = append([]model.Learning{oldEntry}, lf.Learnings...)
	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	if err := yamlutil.AtomicWrite(learningsPath, lf); err != nil {
		t.Fatalf("write modified learnings: %v", err)
	}

	// Read with TTL=72h → old entry (200h ago) should be excluded
	cfg := model.LearningsConfig{InjectCount: 10, TTLHours: 72}
	result, err := learnings.ReadTopKLearnings(d.maestroDir, cfg, time.Now())
	if err != nil {
		t.Fatalf("ReadTopKLearnings: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 learning after TTL filter, got %d", len(result))
	}
	if result[0].Content != "fresh learning" {
		t.Errorf("expected 'fresh learning', got %q", result[0].Content)
	}

	// Verify format includes only the non-expired entry
	formatted := learnings.FormatLearningsSection(result)
	if strings.Contains(formatted, "old learning") {
		t.Error("formatted output should not contain expired learning")
	}
	if !strings.Contains(formatted, "fresh learning") {
		t.Error("formatted output should contain fresh learning")
	}
}

// TestLearningsIntegration_InjectCount_EndToEnd writes multiple learnings via handleResultWrite,
// then reads back with inject_count limit and verifies only the most recent K entries are returned.
func TestLearningsIntegration_InjectCount_EndToEnd(t *testing.T) {
	d := newTestDaemonWithLearnings(t)

	commandID := "cmd_0000000001_abcdef01"

	// Write learnings across multiple result-write calls (separate tasks, same worker)
	for i := 0; i < 3; i++ {
		workerID := "worker1"
		taskID := "task_0000000001_abcdef0" + string(rune('1'+i))
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
			Learnings:  []string{"learning batch " + string(rune('A'+i)) + " item1", "learning batch " + string(rune('A'+i)) + " item2"},
		})

		resp := d.handleResultWrite(req)
		if !resp.Success {
			t.Fatalf("write %d failed: %v", i, resp.Error)
		}
	}

	// Should have 6 total learnings
	lf := readLearningsFile(t, d)
	if len(lf.Learnings) != 6 {
		t.Fatalf("expected 6 total learnings, got %d", len(lf.Learnings))
	}

	// Read with inject_count=2 → should get last 2 entries
	cfg := model.LearningsConfig{InjectCount: 2, TTLHours: 0}
	result, err := learnings.ReadTopKLearnings(d.maestroDir, cfg, time.Now())
	if err != nil {
		t.Fatalf("ReadTopKLearnings: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 learnings with inject_count=2, got %d", len(result))
	}
	// Last 2 entries should be from batch C
	if result[0].Content != "learning batch C item1" {
		t.Errorf("result[0].Content = %q, want %q", result[0].Content, "learning batch C item1")
	}
	if result[1].Content != "learning batch C item2" {
		t.Errorf("result[1].Content = %q, want %q", result[1].Content, "learning batch C item2")
	}

	// Format and verify only injected entries appear
	formatted := learnings.FormatLearningsSection(result)
	if strings.Contains(formatted, "batch A") {
		t.Error("formatted output should not contain batch A (outside inject_count)")
	}
	if strings.Contains(formatted, "batch B") {
		t.Error("formatted output should not contain batch B (outside inject_count)")
	}
	if !strings.Contains(formatted, "batch C") {
		t.Error("formatted output should contain batch C")
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

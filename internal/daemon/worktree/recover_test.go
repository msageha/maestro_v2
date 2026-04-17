package worktree

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// newRecoveryTestManager builds a Manager backed by a temp .maestro directory
// without invoking any git commands. The recovery operations only touch the
// state YAML file, so a real git repo is not required.
func newRecoveryTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	maestroDir := filepath.Join(tmp, ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "worktrees"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := model.WorktreeConfig{Enabled: true, BaseBranch: "main"}
	wm := NewManager(maestroDir, cfg, log.New(os.Stderr, "", 0), core.LogLevelError)
	return wm, maestroDir
}

func writeWorktreeState(t *testing.T, wm *Manager, st *model.WorktreeCommandState) {
	t.Helper()
	if err := wm.saveState(st.CommandID, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
}

func quarantinedState(commandID string) *model.WorktreeCommandState {
	return &model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID:         commandID,
			Branch:            "maestro/" + commandID + "/integration",
			BaseSHA:           "0000000000000000000000000000000000000000",
			Status:            model.IntegrationStatusQuarantined,
			MergeFailureCount: 5,
			QuarantinedAt:     "2026-01-01T00:00:00Z",
			QuarantineReason:  "test reason",
			StallSignaled:     true,
			CreatedAt:         "2026-01-01T00:00:00Z",
			UpdatedAt:         "2026-01-01T00:00:00Z",
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

// readStateFile returns the on-disk YAML bytes for byte-level idempotency
// comparisons.
func readStateFile(t *testing.T, maestroDir, commandID string) []byte {
	t.Helper()
	p := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return b
}

func TestUnquarantine_Success(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_001"
	writeWorktreeState(t, wm, quarantinedState(cmdID))

	if err := wm.Unquarantine(cmdID, "operator unblock"); err != nil {
		t.Fatalf("Unquarantine: %v", err)
	}

	got, err := wm.GetCommandState(cmdID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("status = %s, want failed", got.Integration.Status)
	}
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}
	if got.Integration.QuarantinedAt != "" {
		t.Errorf("QuarantinedAt = %q, want empty", got.Integration.QuarantinedAt)
	}
	if got.Integration.QuarantineReason != "" {
		t.Errorf("QuarantineReason = %q, want empty", got.Integration.QuarantineReason)
	}
	if got.Integration.StallSignaled {
		t.Errorf("StallSignaled = true, want false")
	}
}

func TestUnquarantine_Idempotent(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_test_002"
	writeWorktreeState(t, wm, quarantinedState(cmdID))

	if err := wm.Unquarantine(cmdID, ""); err != nil {
		t.Fatalf("first Unquarantine: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	err := wm.Unquarantine(cmdID, "")
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second Unquarantine err = %v, want ErrAlreadyResolved", err)
	}
	second := readStateFile(t, maestroDir, cmdID)
	if string(first) != string(second) {
		t.Errorf("state file mutated on second call")
	}
}

func TestUnquarantine_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	err := wm.Unquarantine("cmd_test_missing", "")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

func TestResumeMerge_FromConflict(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_003"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 2
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(context.Background(), cmdID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}
	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("status = %s, want failed", got.Integration.Status)
	}
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}
}

func TestResumeMerge_FromFailedWithCount(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_004"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusFailed
	st.Integration.MergeFailureCount = 3
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(context.Background(), cmdID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}
	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}
}

func TestResumeMerge_Idempotent(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_test_005"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(context.Background(), cmdID); err != nil {
		t.Fatalf("first ResumeMerge: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	err := wm.ResumeMerge(context.Background(), cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second ResumeMerge err = %v, want ErrAlreadyResolved", err)
	}
	second := readStateFile(t, maestroDir, cmdID)
	if string(first) != string(second) {
		t.Errorf("state file mutated on second call")
	}
}

func TestResumeMerge_RejectsQuarantined(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_006"
	writeWorktreeState(t, wm, quarantinedState(cmdID))

	err := wm.ResumeMerge(context.Background(), cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("err = %v, want ErrAlreadyResolved", err)
	}
}

func TestResumeMerge_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	err := wm.ResumeMerge(context.Background(), "cmd_test_missing_2")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

// TestResumeMerge_ResetsConflictWorkers verifies that ResumeMerge transitions
// workers in conflict/resolving state back to active, enabling re-merge.
func TestResumeMerge_ResetsConflictWorkers(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_resume_workers"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 2
	st.Integration.QuarantinedAt = ""
	st.Integration.QuarantineReason = ""
	st.Workers = []model.WorktreeState{
		{WorkerID: "worker1", Status: model.WorktreeStatusIntegrated},
		{WorkerID: "worker2", Status: model.WorktreeStatusConflict},
		{WorkerID: "worker3", Status: model.WorktreeStatusResolving},
	}
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(context.Background(), cmdID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	got, err := wm.GetCommandState(cmdID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}

	// Integration should be failed with reset failure count
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("integration status = %s, want failed", got.Integration.Status)
	}
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}

	// worker1 (integrated) should remain unchanged
	for _, ws := range got.Workers {
		switch ws.WorkerID {
		case "worker1":
			if ws.Status != model.WorktreeStatusIntegrated {
				t.Errorf("worker1 status = %s, want integrated", ws.Status)
			}
		case "worker2":
			// conflict → active
			if ws.Status != model.WorktreeStatusActive {
				t.Errorf("worker2 status = %s, want active (reset from conflict)", ws.Status)
			}
		case "worker3":
			// resolving → active
			if ws.Status != model.WorktreeStatusActive {
				t.Errorf("worker3 status = %s, want active (reset from resolving)", ws.Status)
			}
		default:
			t.Errorf("unexpected worker: %s", ws.WorkerID)
		}
	}
}

// TestResumeMerge_IdempotentWithConflictWorkers verifies that a second
// ResumeMerge call after workers have been reset returns ErrAlreadyResolved.
func TestResumeMerge_IdempotentWithConflictWorkers(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_resume_idem_workers"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 1
	st.Integration.QuarantinedAt = ""
	st.Integration.QuarantineReason = ""
	st.Workers = []model.WorktreeState{
		{WorkerID: "worker1", Status: model.WorktreeStatusConflict},
	}
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(context.Background(), cmdID); err != nil {
		t.Fatalf("first ResumeMerge: %v", err)
	}

	// Second call: workers are now active, integration is failed with count=0
	err := wm.ResumeMerge(context.Background(), cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second ResumeMerge err = %v, want ErrAlreadyResolved", err)
	}
}

// TestCommitResolvedWorkerChanges_SkipsSensitiveFiles verifies that
// commitResolvedWorkerChanges does not stage sensitive files (.env, *.key, etc.)
// unlike the old git add -A approach.
func TestCommitResolvedWorkerChanges_SkipsSensitiveFiles(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	cmdID := "cmd_recover_sensitive"
	workerID := "worker1"
	if err := createForCommand(wm, cmdID, []string{workerID}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	ws, err := getState(wm, cmdID, workerID)
	if err != nil {
		t.Fatalf("getState: %v", err)
	}

	// Create normal and sensitive files in the worker worktree.
	normalFile := filepath.Join(ws.Path, "resolved.go")
	if err := os.WriteFile(normalFile, []byte("package resolved\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sensitiveFiles := []string{".env", "server.key", "cert.pem", "credentials.json", "token.secret"}
	for _, f := range sensitiveFiles {
		if err := os.WriteFile(filepath.Join(ws.Path, f), []byte("SENSITIVE\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Set worker to conflict status (prerequisite for commitResolvedWorkerChanges).
	wm.mu.Lock()
	if err := wm.commitResolvedWorkerChanges(ws, cmdID); err != nil {
		wm.mu.Unlock()
		t.Fatalf("commitResolvedWorkerChanges: %v", err)
	}
	wm.mu.Unlock()

	// Verify: check which files were committed.
	committed, err := wm.gitOutputInDir(ws.Path, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil {
		t.Fatalf("diff-tree: %v", err)
	}

	if !strings.Contains(committed, "resolved.go") {
		t.Errorf("resolved.go should be committed, got: %s", committed)
	}
	for _, f := range sensitiveFiles {
		if strings.Contains(committed, f) {
			t.Errorf("sensitive file %q should NOT be committed, got: %s", f, committed)
		}
	}
}

// conflictWorkerState builds a state where a worker is recorded in
// CommitFailedWorkers and the integration is in a recoverable state.
func conflictWorkerState(commandID, workerID string, status model.IntegrationStatus, failures int) *model.WorktreeCommandState {
	st := quarantinedState(commandID)
	st.Integration.Status = status
	st.Integration.MergeFailureCount = failures
	st.Integration.QuarantinedAt = ""
	st.Integration.QuarantineReason = ""
	st.Integration.StallSignaled = false
	st.CommitFailedWorkers = []string{workerID}
	return st
}

func TestResolveConflict_FromConflict_ClearsSignal(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_resolve_001"
	phaseID := "phase_001"
	workerID := "worker_a"
	writeWorktreeState(t, wm, conflictWorkerState(cmdID, workerID, model.IntegrationStatusConflict, 3))

	store := newFakeSignalStore()
	store.put(model.PlannerSignal{
		Kind:                "merge_conflict",
		CommandID:           cmdID,
		PhaseID:             phaseID,
		WorkerID:            workerID,
		ConflictGeneration:  "g1",
		ResolutionState:     "dispatched",
		LastResolutionError: "prior failure",
	})
	wm.SetSignalStore(store)

	if err := wm.ResolveConflict(cmdID, phaseID, workerID); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}

	got, err := wm.GetCommandState(cmdID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if len(got.CommitFailedWorkers) != 0 {
		t.Errorf("CommitFailedWorkers = %v, want empty", got.CommitFailedWorkers)
	}
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("status = %s, want failed", got.Integration.Status)
	}
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}

	// H3: lingering merge_conflict signal must be cleared so a stale
	// ResolutionState=dispatched cannot block re-merge after recovery.
	sig := store.get(cmdID, phaseID, workerID)
	if sig == nil {
		t.Fatal("merge_conflict signal disappeared")
	}
	if sig.ResolutionState != "" {
		t.Errorf("ResolutionState = %q, want empty", sig.ResolutionState)
	}
	if sig.LastResolutionError != "" {
		t.Errorf("LastResolutionError = %q, want empty", sig.LastResolutionError)
	}
}

func TestResolveConflict_Idempotent(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_resolve_002"
	phaseID := "phase_001"
	workerID := "worker_a"
	writeWorktreeState(t, wm, conflictWorkerState(cmdID, workerID, model.IntegrationStatusConflict, 1))

	if err := wm.ResolveConflict(cmdID, phaseID, workerID); err != nil {
		t.Fatalf("first ResolveConflict: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	err := wm.ResolveConflict(cmdID, phaseID, workerID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second ResolveConflict err = %v, want ErrAlreadyResolved", err)
	}
	second := readStateFile(t, maestroDir, cmdID)
	if string(first) != string(second) {
		t.Errorf("state file mutated on second call")
	}
}

func TestResolveConflict_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	err := wm.ResolveConflict("cmd_missing_resolve", "phase_001", "worker_a")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

func TestResolveConflict_FromPartialMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_resolve_004"
	phaseID := "phase_001"
	workerID := "worker_a"
	writeWorktreeState(t, wm, conflictWorkerState(cmdID, workerID, model.IntegrationStatusPartialMerge, 2))

	if err := wm.ResolveConflict(cmdID, phaseID, workerID); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}
	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("status = %s, want failed", got.Integration.Status)
	}
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}
	if len(got.CommitFailedWorkers) != 0 {
		t.Errorf("CommitFailedWorkers = %v, want empty", got.CommitFailedWorkers)
	}
}

// When integration is already Failed, ResolveConflict should still remove the
// worker from CommitFailedWorkers and reset the failure counter without
// requiring a status transition.
func TestResolveConflict_AlreadyFailedJustClearsWorker(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_resolve_005"
	phaseID := "phase_001"
	workerID := "worker_a"
	writeWorktreeState(t, wm, conflictWorkerState(cmdID, workerID, model.IntegrationStatusFailed, 4))

	if err := wm.ResolveConflict(cmdID, phaseID, workerID); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}
	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("status = %s, want failed", got.Integration.Status)
	}
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}
	if len(got.CommitFailedWorkers) != 0 {
		t.Errorf("CommitFailedWorkers = %v, want empty", got.CommitFailedWorkers)
	}
}

// ResolveConflict with no signal store registered must still succeed
// (signal clearing is best-effort and should not be a hard dependency).
func TestResolveConflict_NoSignalStore(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_resolve_006"
	phaseID := "phase_001"
	workerID := "worker_a"
	writeWorktreeState(t, wm, conflictWorkerState(cmdID, workerID, model.IntegrationStatusConflict, 1))

	// Intentionally do not call SetSignalStore.
	if err := wm.ResolveConflict(cmdID, phaseID, workerID); err != nil {
		t.Fatalf("ResolveConflict without signal store: %v", err)
	}
	got, _ := wm.GetCommandState(cmdID)
	if len(got.CommitFailedWorkers) != 0 {
		t.Errorf("CommitFailedWorkers = %v, want empty", got.CommitFailedWorkers)
	}
}

package worktree

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
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
	wm, _ := newRecoveryTestManager(t)
	err := wm.Unquarantine("cmd_test_missing", "")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

func TestResumeMerge_FromConflict(t *testing.T) {
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_003"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 2
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(cmdID); err != nil {
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
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_004"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusFailed
	st.Integration.MergeFailureCount = 3
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(cmdID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}
	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", got.Integration.MergeFailureCount)
	}
}

func TestResumeMerge_Idempotent(t *testing.T) {
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_test_005"
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(cmdID); err != nil {
		t.Fatalf("first ResumeMerge: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	err := wm.ResumeMerge(cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second ResumeMerge err = %v, want ErrAlreadyResolved", err)
	}
	second := readStateFile(t, maestroDir, cmdID)
	if string(first) != string(second) {
		t.Errorf("state file mutated on second call")
	}
}

func TestResumeMerge_RejectsQuarantined(t *testing.T) {
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_test_006"
	writeWorktreeState(t, wm, quarantinedState(cmdID))

	err := wm.ResumeMerge(cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("err = %v, want ErrAlreadyResolved", err)
	}
}

func TestResumeMerge_NoWorktreeState(t *testing.T) {
	wm, _ := newRecoveryTestManager(t)
	err := wm.ResumeMerge("cmd_test_missing_2")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
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
	wm, _ := newRecoveryTestManager(t)
	err := wm.ResolveConflict("cmd_missing_resolve", "phase_001", "worker_a")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

func TestResolveConflict_FromPartialMerge(t *testing.T) {
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

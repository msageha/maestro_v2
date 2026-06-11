package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

func TestBuildMergeMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		workerID       string
		workerPurposes map[string]string
		want           string
	}{
		{
			name:           "with purpose",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": "add login API"},
			want:           "merge: add login API",
		},
		{
			name:           "nil purposes map",
			workerID:       "worker1",
			workerPurposes: nil,
			want:           "merge: worker1 changes",
		},
		{
			name:           "worker not in map",
			workerID:       "worker2",
			workerPurposes: map[string]string{"worker1": "some purpose"},
			want:           "merge: worker2 changes",
		},
		{
			name:           "empty purpose",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": ""},
			want:           "merge: worker1 changes",
		},
		{
			name:           "long purpose truncated to 72 chars",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": strings.Repeat("a", 100)},
			want:           "merge: " + strings.Repeat("a", 65),
		},
		{
			name:           "multiline purpose uses first line only",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": "first line\nsecond line"},
			want:           "merge: first line",
		},
		{
			name:           "no maestro prefix",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": "add feature"},
			want:           "merge: add feature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildMergeMessage(tt.workerID, tt.workerPurposes)
			if got != tt.want {
				t.Errorf("buildMergeMessage() = %q, want %q", got, tt.want)
			}
			if len(got) > mergePublishMaxLen {
				t.Errorf("buildMergeMessage() length %d exceeds max %d", len(got), mergePublishMaxLen)
			}
			if strings.Contains(got, "[maestro]") {
				t.Errorf("buildMergeMessage() should not contain [maestro] prefix")
			}
		})
	}
}

func TestBuildPublishMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		publishMessage string
		baseBranch     string
		want           string
	}{
		{
			name:           "with content",
			publishMessage: "ユーザー認証機能を実装する",
			baseBranch:     "main",
			want:           "publish: ユーザー認証機能を実装する",
		},
		{
			name:           "empty content fallback",
			publishMessage: "",
			baseBranch:     "main",
			want:           "publish: integrate changes to main",
		},
		{
			name:           "long content truncated",
			publishMessage: strings.Repeat("b", 100),
			baseBranch:     "main",
			want:           "publish: " + strings.Repeat("b", 63),
		},
		{
			name:           "multiline content uses first line",
			publishMessage: "first line\nsecond line\nthird line",
			baseBranch:     "main",
			want:           "publish: first line",
		},
		{
			name:           "no maestro prefix",
			publishMessage: "deploy pipeline",
			baseBranch:     "main",
			want:           "publish: deploy pipeline",
		},
		{
			name:           "different base branch in fallback",
			publishMessage: "",
			baseBranch:     "develop",
			want:           "publish: integrate changes to develop",
		},
		{
			// Rune-based budget: "publish: " (9 runes) + 7 "あ" + 50 'x'
			// = 66 runes ≤ 72, so the message survives intact even though
			// its byte length (80B) exceeds 72. The previous byte-based cut
			// destroyed most of a Japanese summary (E2E 2026-06-11:
			// "publish: 前回のコマンド cmd_xxx が parti").
			name:           "japanese-ascii mix within rune budget is kept",
			publishMessage: strings.Repeat("あ", 7) + strings.Repeat("x", 50),
			baseBranch:     "main",
			want:           "publish: " + strings.Repeat("あ", 7) + strings.Repeat("x", 50),
		},
		{
			// Over the rune budget: "publish: " (9 runes) + 70 "あ" = 79
			// runes → truncated to 72 runes = prefix + 63 "あ".
			name:           "long japanese truncated by rune count",
			publishMessage: strings.Repeat("あ", 70),
			baseBranch:     "main",
			want:           "publish: " + strings.Repeat("あ", 63),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildPublishMessage(tt.publishMessage, tt.baseBranch)
			if got != tt.want {
				t.Errorf("buildPublishMessage() = %q, want %q", got, tt.want)
			}
			if utf8.RuneCountInString(got) > mergePublishMaxLen {
				t.Errorf("buildPublishMessage() rune count %d exceeds max %d", utf8.RuneCountInString(got), mergePublishMaxLen)
			}
			if !utf8.ValidString(got) {
				t.Errorf("buildPublishMessage() produced invalid UTF-8: %q", got)
			}
			if strings.Contains(got, "[maestro]") {
				t.Errorf("buildPublishMessage() should not contain [maestro] prefix")
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
		body   string
		maxLen int
		want   string
	}{
		{
			name:   "short message unchanged",
			prefix: "merge: ",
			body:   "hello",
			maxLen: 72,
			want:   "merge: hello",
		},
		{
			name:   "truncated at maxLen",
			prefix: "merge: ",
			body:   strings.Repeat("x", 100),
			maxLen: 20,
			want:   "merge: xxxxxxxxxxxxx",
		},
		{
			name:   "empty body returns prefix only",
			prefix: "merge: ",
			body:   "",
			maxLen: 72,
			want:   "merge: ",
		},
		{
			name:   "whitespace-only body returns prefix",
			prefix: "publish: ",
			body:   "   \t  ",
			maxLen: 72,
			want:   "publish: ",
		},
		{
			name:   "newline takes first line",
			prefix: "p: ",
			body:   "line1\nline2",
			maxLen: 72,
			want:   "p: line1",
		},
		{
			// Rune-based cut: "p: " (3 runes) + "あいう" (3 runes) = 6 runes;
			// maxLen=4 keeps the prefix plus one Japanese character.
			name:   "japanese truncated by rune count",
			prefix: "p: ",
			body:   "あいう",
			maxLen: 4,
			want:   "p: あ",
		},
		{
			// Within the rune budget: 6 runes ≤ 9 even though the byte
			// length (12B) exceeds 9 — byte length no longer matters.
			name:   "japanese within rune budget is kept",
			prefix: "p: ",
			body:   "あいう",
			maxLen: 9,
			want:   "p: あいう",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateMessage(tt.prefix, tt.body, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateMessage() = %q, want %q", got, tt.want)
			}
			if tt.maxLen > 0 && utf8.RuneCountInString(got) > tt.maxLen {
				t.Errorf("truncateMessage() rune count %d exceeds max %d", utf8.RuneCountInString(got), tt.maxLen)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateMessage() produced invalid UTF-8: %q", got)
			}
		})
	}
}

// TestMergeToIntegration_PathGuardRejectsEscape verifies that MergeToIntegration
// refuses to operate when the integration worktree path escapes the project root
// (e.g. via symlink). This is defense-in-depth for the recovery paths that use
// git reset --hard + clean -fd.
func TestMergeToIntegration_PathGuardRejectsEscape(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_pathguard"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Replace the integration worktree with a symlink escaping projectRoot.
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")

	// Remove the real git worktree first (gitRun does not take the mutex).
	_ = wm.gitRun("worktree", "remove", "--force", integrationPath)
	_ = os.RemoveAll(integrationPath)

	// Create an outside directory and symlink to it.
	outside := t.TempDir()
	if err := os.Symlink(outside, integrationPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// MergeToIntegration should refuse due to pathGuard.
	_, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err == nil {
		t.Fatal("expected path guard error, got nil")
	}
	if !strings.Contains(err.Error(), "merge to integration refused") {
		t.Errorf("expected path guard error message, got: %v", err)
	}
}

// --- M3 Test: MergeFailureCount reset independent of setIntegrationStatus ---

// TestMergeToIntegration_MergeFailureCountResetOnSuccess verifies that
// MergeFailureCount is reset to 0 on a successful merge even when reaching
// this point after a previous failure count > 0.
func TestMergeToIntegration_MergeFailureCountResetOnSuccess(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_m3_reset"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Manually set a non-zero MergeFailureCount to simulate prior failures
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	state.Integration.MergeFailureCount = 2
	wm.mu.Lock()
	if err := wm.saveState(commandID, state); err != nil {
		wm.mu.Unlock()
		t.Fatalf("saveState: %v", err)
	}
	wm.mu.Unlock()

	// Create a file and commit so there is something to merge
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "m3_file.txt"), []byte("m3"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add m3_file.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge should succeed
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Verify MergeFailureCount is reset to 0
	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState after merge: %v", err)
	}
	if state.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0", state.Integration.MergeFailureCount)
	}
}

// --- M1 Test: SyncFromIntegration merge --abort fallback recovery ---

// TestSyncFromIntegration_MergeConflictAbortRecovery verifies that
// SyncFromIntegration properly recovers when merge --abort would be needed.
// This test creates a real merge conflict during sync and verifies the worker
// is left in a clean state after the conflict is handled.
func TestSyncFromIntegration_MergeConflictRecovery(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_m1_recovery"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1 modifies README.md and commits
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 changes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "w1 edit README"); err != nil {
		t.Fatal(err)
	}

	// Merge worker1 to integration
	if _, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}

	// Worker2 also modifies README.md (conflicting)
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 changes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "w2 edit README"); err != nil {
		t.Fatal(err)
	}

	// Sync integration→worker2: this should conflict (README.md modified both ways)
	if err := wm.SyncFromIntegration(commandID, []string{"worker2"}); err != nil {
		t.Fatalf("SyncFromIntegration failed: %v", err)
	}

	// Worker2 should be in conflict state
	ws2, err := getState(wm, commandID, "worker2")
	if err != nil {
		t.Fatalf("getState(worker2): %v", err)
	}
	if ws2.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status = %q, want conflict", ws2.Status)
	}

	// Worker2 worktree should be clean (merge was aborted)
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wt2
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("worker2 worktree should be clean after merge abort, got: %s", out)
	}
}

// TestMergeToIntegration_NoCommitsRevertsStatus verifies that when all workers
// have no commits to merge, the integration status is reverted to the pre-merge
// status (not set to Merged) to prevent a no-op publish.
func TestMergeToIntegration_NoCommitsRevertsStatus(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_no_commits"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Don't commit anything in any worker — all have zero commits beyond base.

	// Verify initial status is Created
	stateBefore, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if stateBefore.Integration.Status != model.IntegrationStatusCreated {
		t.Fatalf("initial status = %s, want created", stateBefore.Integration.Status)
	}

	// Merge — should find no commits and revert to Created
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Verify status was NOT set to Merged
	stateAfter, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState after: %v", err)
	}
	if stateAfter.Integration.Status == model.IntegrationStatusMerged {
		t.Error("integration status should NOT be Merged when no commits were merged")
	}
	// It should be reverted to the original status (Created)
	if stateAfter.Integration.Status != model.IntegrationStatusCreated {
		t.Errorf("integration status = %s, want created (reverted)", stateAfter.Integration.Status)
	}
}

// TestSyncFromIntegration_CapturesPreMergeHEAD verifies that
// SyncFromIntegration captures the pre-merge HEAD for recovery purposes.
// The test verifies that after a failed sync, the worktree HEAD is preserved.
func TestSyncFromIntegration_PreservesWorktreeOnFailure(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_m1_prehead"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1 creates a unique file and commits
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "w1 readme"); err != nil {
		t.Fatal(err)
	}

	// Merge worker1 to integration
	if _, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}

	// Worker2 modifies the same file differently
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}

	// Get worker2's HEAD before sync
	headBefore := gitRevParse(t, wt2, "HEAD")

	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "w2 readme"); err != nil {
		t.Fatal(err)
	}

	headAfterCommit := gitRevParse(t, wt2, "HEAD")

	// Sync will conflict
	if err := wm.SyncFromIntegration(commandID, []string{"worker2"}); err != nil {
		t.Fatalf("SyncFromIntegration: %v", err)
	}

	// Worker2's HEAD should be preserved (same as after commit, not moved)
	headAfterSync := gitRevParse(t, wt2, "HEAD")
	if headAfterSync != headAfterCommit {
		t.Errorf("worker2 HEAD changed after failed sync: before=%s after=%s", headAfterCommit, headAfterSync)
	}
	if headAfterSync == headBefore {
		t.Errorf("worker2 HEAD should be different from initial (commit was made)")
	}
}

// TestMergeToIntegration_SkipAlreadyIntegrated verifies that a worker already
// in "integrated" status is not re-merged during a partial_merge recovery pass.
// Worker1 is merged first (→integrated), then on the second MergeToIntegration
// call (simulating re-merge after partial_merge), worker1 should be skipped
// while worker2 is merged normally.
func TestMergeToIntegration_SkipAlreadyIntegrated(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_skip_integrated"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1: create and commit a file
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "w1.txt"), []byte("worker1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "w1 add file"); err != nil {
		t.Fatal(err)
	}

	// Worker2: create and commit a different file
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "w2.txt"), []byte("worker2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "w2 add file"); err != nil {
		t.Fatal(err)
	}

	// First merge: only worker1
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil)
	if err != nil {
		t.Fatalf("first MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts on first merge: %v", conflicts)
	}

	// Verify worker1 is now integrated
	ws1, err := getState(wm, commandID, "worker1")
	if err != nil {
		t.Fatalf("getState(worker1): %v", err)
	}
	if ws1.Status != model.WorktreeStatusIntegrated {
		t.Fatalf("worker1 status = %q, want integrated", ws1.Status)
	}

	// Record integration HEAD before second merge
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	headBeforeSecondMerge := gitRevParse(t, integrationPath, "HEAD")

	// Second merge: both workers (simulates re-merge after partial_merge)
	conflicts, err = wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("second MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts on second merge: %v", conflicts)
	}

	// Integration HEAD should have advanced (worker2 was merged)
	headAfterSecondMerge := gitRevParse(t, integrationPath, "HEAD")
	if headAfterSecondMerge == headBeforeSecondMerge {
		t.Error("integration HEAD should have advanced after merging worker2")
	}

	// Worker1 should still be integrated (not re-merged)
	ws1After, err := getState(wm, commandID, "worker1")
	if err != nil {
		t.Fatalf("getState(worker1) after: %v", err)
	}
	if ws1After.Status != model.WorktreeStatusIntegrated {
		t.Errorf("worker1 status after second merge = %q, want integrated", ws1After.Status)
	}

	// Worker2 should now be integrated
	ws2After, err := getState(wm, commandID, "worker2")
	if err != nil {
		t.Fatalf("getState(worker2) after: %v", err)
	}
	if ws2After.Status != model.WorktreeStatusIntegrated {
		t.Errorf("worker2 status after second merge = %q, want integrated", ws2After.Status)
	}

	// Final integration status should be Merged (all workers integrated, no conflicts)
	cmdState, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if cmdState.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged", cmdState.Integration.Status)
	}
}

// TestMergeToIntegration_SkipConflictResolving verifies that workers in
// conflict or resolving status are skipped during MergeToIntegration, avoiding
// the invalid conflict→conflict worktree transition. When all workers are
// conflict-skipped, the integration status should revert to pre-merge.
func TestMergeToIntegration_SkipConflictResolving(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_skip_conflict"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1: create a file and commit
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte("worker1 version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "w1 add shared.txt"); err != nil {
		t.Fatal(err)
	}

	// Worker2: create same file with different content (will conflict)
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("worker2 version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "w2 add shared.txt"); err != nil {
		t.Fatal(err)
	}

	// First merge: merges both, worker1 succeeds, worker2 conflicts
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("first MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker2" {
		t.Fatalf("expected 1 conflict for worker2, got: %v", conflicts)
	}

	// Verify worker2 is in conflict state
	ws2, err := getState(wm, commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if ws2.Status != model.WorktreeStatusConflict {
		t.Fatalf("worker2 status = %q, want conflict", ws2.Status)
	}

	// Integration should be partial_merge (worker1 succeeded, worker2 conflicted)
	cmdState, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatal(err)
	}
	if cmdState.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Fatalf("integration status = %q, want partial_merge", cmdState.Integration.Status)
	}

	// Second merge: re-merge both workers. Worker1 is integrated (skipped),
	// worker2 is conflict (skipped by new logic). No invalid_worktree_transition.
	conflicts, err = wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("second MergeToIntegration should not error: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no new conflicts, got: %v", conflicts)
	}

	// Worker2 should still be in conflict (not re-merged, not invalid transition)
	ws2After, err := getState(wm, commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if ws2After.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status after second merge = %q, want conflict (should be skipped)", ws2After.Status)
	}

	// Integration should revert to pre-merge status since only conflict workers remain
	cmdStateAfter, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-merge status was partial_merge; with only conflict-skipped workers,
	// it should revert to partial_merge (not create new conflict/merged status)
	if cmdStateAfter.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Errorf("integration status after second merge = %q, want partial_merge (reverted)", cmdStateAfter.Integration.Status)
	}
}

// --- Problem 1: SHA validation in forwardMerge ---

// TestForwardMerge_RejectsDanglingBaseSHA verifies that PublishToBase returns a
// clear error when the base branch points to a non-existent commit object.
func TestForwardMerge_RejectsDanglingBaseSHA(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_dangling_sha"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker commits something and merge to integration
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "file.txt"), []byte("content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add file"); err != nil {
		t.Fatal(err)
	}
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) > 0 {
		t.Fatal("unexpected conflicts")
	}

	// Write a dangling SHA directly to the main branch ref file.
	// git rev-parse will return this SHA (it doesn't verify object existence),
	// but cat-file -e should reject it.
	danglingRef := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	mainRefPath := filepath.Join(projectRoot, ".git", "refs", "heads", "main")
	if err := os.WriteFile(mainRefPath, []byte(danglingRef+"\n"), 0644); err != nil {
		t.Fatalf("write dangling ref: %v", err)
	}

	// PublishToBase should fail with a clear error about missing SHA
	err = wm.PublishToBase(commandID, "test publish")
	if err == nil {
		t.Fatal("expected error for dangling base SHA")
	}
	if !strings.Contains(err.Error(), "not found as commit in repository") {
		t.Errorf("expected clear error about SHA not found, got: %v", err)
	}
}

// --- Problem 2: temp branch cleanup ---

// TestPublishToBase_NoStaleTempBranch verifies that the temporary publish
// branch (maestro/<cmd>/_publish) is cleaned up after a successful publish.
func TestPublishToBase_NoStaleTempBranch(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_no_stale_temp"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "file.txt"), []byte("content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add file"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatal(err)
	}

	if err := wm.PublishToBase(commandID, "test publish"); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Verify no temp publish branch remains
	tempBranch := "maestro/" + commandID + "/_publish"
	cmd := exec.Command("git", "-C", projectRoot, "rev-parse", "--verify", "--quiet",
		"refs/heads/"+tempBranch)
	if err := cmd.Run(); err == nil {
		t.Errorf("temp branch %s should not exist after successful publish", tempBranch)
	}
}

// TestPublishToBase_TempBranchCleanedOnMergeConflict verifies that the
// temporary publish branch is cleaned up when publish fails due to a merge
// conflict within performPublishMerge (not the forward-merge path).
func TestPublishToBase_TempBranchCleanedOnMergeConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_temp_merge_conflict"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker modifies README.md and merge to integration
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "modify README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatal(err)
	}

	// Advance base with conflicting change
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("base version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitAdd(t, projectRoot, "README.md")
	gitCommit(t, projectRoot, "modify README.md on main (conflicting)")

	// PublishToBase fails (forward-merge conflict — temp branch not created in this path).
	// Even if it fails, verify no stale temp branch.
	_ = wm.PublishToBase(commandID, "test publish")

	tempBranch := "maestro/" + commandID + "/_publish"
	cmd := exec.Command("git", "-C", projectRoot, "rev-parse", "--verify", "--quiet",
		"refs/heads/"+tempBranch)
	if err := cmd.Run(); err == nil {
		t.Errorf("temp branch %s should not exist after publish failure", tempBranch)
	}
}

// TestPublishToBase_NoFalsePositiveStash verifies that PublishToBase does not
// create a spurious stash ref under refs/maestro/pre-publish-stash/. Before the
// fix, update-ref advanced HEAD without syncing the index/working tree, causing
// git stash create to see a false diff and accumulate orphaned stash refs.
func TestPublishToBase_NoFalsePositiveStash(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_no_false_stash"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker commits a file and merge to integration
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "feature.txt"), []byte("new feature\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatal(err)
	}

	// Publish to base
	if err := wm.PublishToBase(commandID, "publish feature"); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Verify no stash ref was created (false positive eliminated)
	stashRef := "refs/maestro/pre-publish-stash/" + commandID
	cmd := exec.Command("git", "-C", projectRoot, "rev-parse", "--verify", "--quiet", stashRef)
	if err := cmd.Run(); err == nil {
		t.Errorf("stash ref %s should not exist after clean publish (false positive stash)", stashRef)
	}
}

// TestBytesContainConflictMarkers: any line starting with `<<<<<<<` or
// `>>>>>>>` is an unresolved conflict (partial resolutions stay
// fail-closed). A lone `=======` line is legitimate content (setext
// heading underline) and must not block staging — every real conflict
// block carries the outer markers, so this loses no real conflicts.
func TestBytesContainConflictMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"empty", []byte(""), false},
		{"clean text", []byte("hello\nworld\n"), false},
		{"full conflict block", []byte("a\n<<<<<<< HEAD\nx\n=======\ny\n>>>>>>> branch\nb\n"), true},
		{"conflict block at file head", []byte("<<<<<<< HEAD\nx\n=======\ny\n>>>>>>> branch\n"), true},
		{"conflict block CRLF", []byte("<<<<<<< HEAD\r\nx\r\n=======\r\ny\r\n>>>>>>> branch\r\n"), true},
		// Partial resolutions that still carry an outer marker stay blocked.
		{"opening marker only", []byte("a\n<<<<<<< HEAD\nx\n"), true},
		{"closing marker only", []byte("a\n>>>>>>> branch\n"), true},
		{"reversed order", []byte(">>>>>>> b\n=======\n<<<<<<< a\n"), true},
		// Divider lookalikes are legitimate content.
		{"setext heading underline", []byte("Title\n=======\nbody\n"), false},
		{"divider at file head", []byte("=======\nrest\n"), false},
		{"divider mid-line no newline", []byte("foo=======bar"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bytesContainConflictMarkers(tc.data); got != tc.want {
				t.Errorf("bytesContainConflictMarkers(%q) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

// TestTryCompleteInterruptedPublishSync reproduces the crash window between
// the base update-ref and the project-root sync: base ref advanced to the
// published merge, index/worktree still at the pre-publish tree, marker ref
// present. The repair must complete the sync; genuine staged edits must
// refuse it.
func TestTryCompleteInterruptedPublishSync(t *testing.T) {
	t.Parallel()

	setupCrashState := func(t *testing.T) (string, *Manager, string) {
		t.Helper()
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)
		commandID := "cmd_pubsync"

		mustGit := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = projectRoot
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}

		baseBranch := mustGit("symbolic-ref", "--short", "HEAD")
		oldSHA := mustGit("rev-parse", "HEAD")

		// Build the "published merge" on a side branch, then move the base
		// ref to it WITHOUT syncing the checkout — the crash state.
		mustGit("checkout", "-b", "published_tmp")
		if err := os.WriteFile(filepath.Join(projectRoot, "published.txt"), []byte("published\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit("add", "-A")
		mustGit("commit", "-m", "published change")
		mergeSHA := mustGit("rev-parse", "HEAD")
		mustGit("checkout", baseBranch)
		mustGit("update-ref", "refs/heads/"+baseBranch, mergeSHA, oldSHA)
		mustGit("update-ref", publishSyncPendingRef(commandID), oldSHA)

		// Sanity: root now looks "dirty" (index vs advanced HEAD).
		if out := mustGit("status", "--porcelain", "--untracked-files=no"); out == "" {
			t.Fatal("setup: expected stale-root crash state to look dirty")
		}
		return projectRoot, wm, commandID
	}

	t.Run("completes interrupted sync", func(t *testing.T) {
		t.Parallel()
		projectRoot, wm, commandID := setupCrashState(t)

		if got := wm.tryCompleteInterruptedPublishSync(commandID); got != publishSyncRepairCompleted {
			t.Fatalf("expected crash-signature repair to complete, got outcome %d", got)
		}
		if _, err := os.Stat(filepath.Join(projectRoot, "published.txt")); err != nil {
			t.Errorf("published content should be materialised after resync: %v", err)
		}
		statusCmd := exec.Command("git", "status", "--porcelain", "--untracked-files=no")
		statusCmd.Dir = projectRoot
		out, err := statusCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("root should be clean after resync, got:\n%s", out)
		}
		verify := exec.Command("git", "rev-parse", "--verify", "-q", publishSyncPendingRef(commandID))
		verify.Dir = projectRoot
		if verify.Run() == nil {
			t.Error("sync-pending marker should be deleted after repair")
		}
	})

	t.Run("refuses when operator staged edits", func(t *testing.T) {
		t.Parallel()
		projectRoot, wm, commandID := setupCrashState(t)

		// Stage a genuine operator edit on top of the crash state.
		if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("operator edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		add := exec.Command("git", "add", "README.md")
		add.Dir = projectRoot
		if out, err := add.CombinedOutput(); err != nil {
			t.Fatalf("git add: %v\n%s", err, out)
		}

		if got := wm.tryCompleteInterruptedPublishSync(commandID); got != publishSyncRepairRefused {
			t.Fatalf("repair must refuse when the index holds operator edits (reset --hard would destroy them), got outcome %d", got)
		}
		data, err := os.ReadFile(filepath.Join(projectRoot, "README.md"))
		if err != nil || string(data) != "operator edit\n" {
			t.Errorf("operator edit must survive (data=%q err=%v)", data, err)
		}
	})
}

// TestMergeToIntegration_DefersWhenForwardMergeInFlight: while the
// integration worktree has MERGE_HEAD from an in-flight base→integration
// forward merge (publish conflict resolution pending), MergeToIntegration
// must defer — NOT run its pre-merge reset --hard + clean -fd, which would
// destroy MERGE_HEAD and the uncommitted resolution edits that
// reuseInFlightForwardMerge stages on the next publish retry.
func TestMergeToIntegration_DefersWhenForwardMergeInFlight(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_fwd_busy"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}
	ip := wm.integrationWorktreePath(commandID)

	mustGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Manufacture an in-flight merge: a side branch and the integration
	// branch each gain a commit, then `merge --no-commit --no-ff` leaves
	// MERGE_HEAD in the integration worktree.
	mustGit(ip, "branch", "side")
	if err := os.WriteFile(filepath.Join(ip, "integration.txt"), []byte("integration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(ip, "add", "-A")
	mustGit(ip, "commit", "-m", "integration side")
	mustGit(ip, "checkout", "side")
	if err := os.WriteFile(filepath.Join(ip, "side.txt"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(ip, "add", "-A")
	mustGit(ip, "commit", "-m", "side branch")
	mustGit(ip, "checkout", "-")
	mustGit(ip, "merge", "--no-commit", "--no-ff", "side")
	if !runGitOK(t, ip, "rev-parse", "--verify", "-q", "MERGE_HEAD") {
		t.Fatal("setup: MERGE_HEAD should exist after merge --no-commit")
	}

	// Uncommitted "resolution edit" that must survive.
	resolutionFile := filepath.Join(ip, "resolution.txt")
	if err := os.WriteFile(resolutionFile, []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil)
	if !errors.Is(err, ErrIntegrationBusyForwardMerge) {
		t.Fatalf("MergeToIntegration error = %v, want ErrIntegrationBusyForwardMerge", err)
	}

	if !runGitOK(t, ip, "rev-parse", "--verify", "-q", "MERGE_HEAD") {
		t.Error("MERGE_HEAD must survive a deferred MergeToIntegration")
	}
	if data, rErr := os.ReadFile(resolutionFile); rErr != nil || string(data) != "resolved\n" {
		t.Errorf("uncommitted resolution edit must survive (data=%q err=%v)", data, rErr)
	}
}

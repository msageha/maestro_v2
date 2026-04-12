package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildPublishMessage(tt.publishMessage, tt.baseBranch)
			if got != tt.want {
				t.Errorf("buildPublishMessage() = %q, want %q", got, tt.want)
			}
			if len(got) > mergePublishMaxLen {
				t.Errorf("buildPublishMessage() length %d exceeds max %d", len(got), mergePublishMaxLen)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateMessage(tt.prefix, tt.body, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateMessage() = %q, want %q", got, tt.want)
			}
			if tt.maxLen > 0 && len(got) > tt.maxLen {
				t.Errorf("truncateMessage() length %d exceeds max %d", len(got), tt.maxLen)
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
	projectRoot := initTestGitRepo(t)
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
	_, err := wm.MergeToIntegration(commandID, workers, nil)
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
	projectRoot := initTestGitRepo(t)
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
	conflicts, err := wm.MergeToIntegration(commandID, workers, nil)
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
	projectRoot := initTestGitRepo(t)
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
	if _, err := wm.MergeToIntegration(commandID, []string{"worker1"}, nil); err != nil {
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

// TestSyncFromIntegration_CapturesPreMergeHEAD verifies that
// SyncFromIntegration captures the pre-merge HEAD for recovery purposes.
// The test verifies that after a failed sync, the worktree HEAD is preserved.
func TestSyncFromIntegration_PreservesWorktreeOnFailure(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
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
	if _, err := wm.MergeToIntegration(commandID, []string{"worker1"}, nil); err != nil {
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

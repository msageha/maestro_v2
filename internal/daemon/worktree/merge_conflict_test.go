package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestMergeConflict_Detection verifies that MergeToIntegration correctly detects
// a conflict when two workers modify the same file with different content.
// Verifies: conflict count, correct worker identified, integration worktree clean after abort.
func TestMergeConflict_Detection(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_mc_detect"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1: %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2: %v", err)
	}

	// Both workers create the same file with different content
	if err := os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte("content from worker1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add shared.txt"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("content from worker2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add shared.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge both — sorted order: worker1 first (succeeds), worker2 conflicts
	conflicts, err := wm.MergeToIntegration(commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	// Exactly one conflict on worker2
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	if conflicts[0].WorkerID != "worker2" {
		t.Errorf("conflict worker = %q, want worker2", conflicts[0].WorkerID)
	}

	// Verify integration worktree is clean (merge was aborted properly)
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = integrationPath
	statusOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status in integration: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("integration worktree should be clean after conflict abort, got: %s", strings.TrimSpace(string(statusOut)))
	}

	// Verify no MERGE_HEAD remains in integration worktree
	cmd = exec.Command("git", "rev-parse", "MERGE_HEAD")
	cmd.Dir = integrationPath
	if _, mergeErr := cmd.CombinedOutput(); mergeErr == nil {
		t.Error("MERGE_HEAD should not exist in integration worktree after merge abort")
	}
}

// TestMergeConflict_EscalationFlow verifies that after conflict detection,
// the proper state is established for downstream escalation (R7 reconciler):
// - Worker status = WorktreeStatusConflict
// - Integration status = partial_merge (when some workers merged successfully)
// - MergeConflict struct is fully populated with WorkerID, ConflictFiles, and Message
func TestMergeConflict_EscalationFlow(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_mc_escalation"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1: %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2: %v", err)
	}

	// Create conflict on shared.txt
	if err := os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte("worker1 version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 shared.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("worker2 version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 shared.txt"); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	// Verify MergeConflict struct is populated for escalation
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	mc := conflicts[0]
	if mc.WorkerID != "worker2" {
		t.Errorf("conflict WorkerID = %q, want worker2", mc.WorkerID)
	}
	if mc.Message == "" {
		t.Error("conflict Message should not be empty")
	}
	if len(mc.ConflictFiles) == 0 {
		t.Error("ConflictFiles should not be empty")
	}

	// Verify worker2 status = conflict (enables R7 reconciler to detect and escalate)
	ws2, err := getState(wm, commandID, "worker2")
	if err != nil {
		t.Fatalf("GetState worker2: %v", err)
	}
	if ws2.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status = %q, want %q", ws2.Status, model.WorktreeStatusConflict)
	}

	// Verify worker1 was integrated successfully (partial merge preserves successes)
	ws1, err := getState(wm, commandID, "worker1")
	if err != nil {
		t.Fatalf("GetState worker1: %v", err)
	}
	if ws1.Status != model.WorktreeStatusIntegrated {
		t.Errorf("worker1 status = %q, want %q", ws1.Status, model.WorktreeStatusIntegrated)
	}

	// Verify integration status = partial_merge (worker1 merged, worker2 conflicted)
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPartialMerge)
	}

	// Verify MergeFailureCount was reset (conflict is a clean outcome, not an unrecoverable failure)
	if state.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0 (conflict is not an unrecoverable failure)", state.Integration.MergeFailureCount)
	}
}

// TestMergeConflict_RefExtraction verifies that base/ours/theirs SHA refs
// are correctly extracted from conflicting files during merge.
// All three refs should be valid SHA-1 hashes and mutually distinct.
func TestMergeConflict_RefExtraction(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_mc_refs"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1: %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2: %v", err)
	}

	// Modify README.md (exists in base commit) differently in each worker
	// to ensure all 3 stages (base, ours, theirs) exist in the conflict index.
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 modification\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 modify README.md"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 modification\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 modify README.md"); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	mc := conflicts[0]
	shaPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)

	// Verify base/ours/theirs refs are valid SHA-1 hashes
	if !shaPattern.MatchString(mc.BaseRef) {
		t.Errorf("BaseRef = %q, want valid 40-char hex SHA", mc.BaseRef)
	}
	if !shaPattern.MatchString(mc.OursRef) {
		t.Errorf("OursRef = %q, want valid 40-char hex SHA", mc.OursRef)
	}
	if !shaPattern.MatchString(mc.TheirsRef) {
		t.Errorf("TheirsRef = %q, want valid 40-char hex SHA", mc.TheirsRef)
	}

	// All three refs should be distinct (base, worker1, worker2 have different content)
	if mc.BaseRef == mc.OursRef {
		t.Errorf("BaseRef and OursRef should differ: both = %s", mc.BaseRef)
	}
	if mc.BaseRef == mc.TheirsRef {
		t.Errorf("BaseRef and TheirsRef should differ: both = %s", mc.BaseRef)
	}
	if mc.OursRef == mc.TheirsRef {
		t.Errorf("OursRef and TheirsRef should differ: both = %s", mc.OursRef)
	}
}

// TestMergeConflict_BinaryFile verifies that binary file conflicts correctly
// extract non-empty base/ours/theirs refs (W5: binary files must not produce
// empty refs when the base version exists).
func TestMergeConflict_BinaryFile(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Add a binary file to the base commit so all 3 stages will exist during conflict
	binaryBase := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00}
	if err := os.WriteFile(filepath.Join(projectRoot, "image.png"), binaryBase, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "image.png"},
		{"git", "commit", "-m", "add binary file to base"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	commandID := "cmd_mc_binary"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1: %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2: %v", err)
	}

	// Modify binary file differently in each worker
	binary1 := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x01, 0x01}
	binary2 := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x02, 0x02}

	if err := os.WriteFile(filepath.Join(wt1, "image.png"), binary1, 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 modify binary"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "image.png"), binary2, 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 modify binary"); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	mc := conflicts[0]
	shaPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)

	// W5: refs must not be empty for binary file conflicts when base exists
	if mc.BaseRef == "" {
		t.Error("BaseRef should not be empty for binary file conflict")
	}
	if mc.OursRef == "" {
		t.Error("OursRef should not be empty for binary file conflict")
	}
	if mc.TheirsRef == "" {
		t.Error("TheirsRef should not be empty for binary file conflict")
	}

	// Validate SHA format when non-empty
	for _, ref := range []struct {
		name, val string
	}{
		{"BaseRef", mc.BaseRef},
		{"OursRef", mc.OursRef},
		{"TheirsRef", mc.TheirsRef},
	} {
		if ref.val != "" && !shaPattern.MatchString(ref.val) {
			t.Errorf("%s = %q, want valid 40-char hex SHA", ref.name, ref.val)
		}
	}

	// Verify conflict file is correctly identified as image.png
	found := false
	for _, f := range mc.ConflictFiles {
		if f == "image.png" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ConflictFiles = %v, want to contain 'image.png'", mc.ConflictFiles)
	}
}

// TestMergeConflict_ConflictFilesAccuracy verifies that the conflict file list
// extracted by getConflictFilesInDir exactly matches the files that git reports
// as conflicting — no more, no less.
func TestMergeConflict_ConflictFilesAccuracy(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create base files that both workers will modify
	for _, name := range []string{"file_a.txt", "file_b.txt", "file_c.txt"} {
		if err := os.WriteFile(filepath.Join(projectRoot, name), []byte("base content\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"git", "add", "file_a.txt", "file_b.txt", "file_c.txt"},
		{"git", "commit", "-m", "add base files"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	commandID := "cmd_mc_files"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1: %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2: %v", err)
	}

	// Worker1: modify file_a.txt, file_b.txt (not file_c.txt), add unique1.txt
	if err := os.WriteFile(filepath.Join(wt1, "file_a.txt"), []byte("worker1 a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "file_b.txt"), []byte("worker1 b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "unique1.txt"), []byte("unique to worker1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 changes"); err != nil {
		t.Fatal(err)
	}

	// Worker2: modify file_a.txt, file_b.txt differently (conflicts); leave file_c.txt alone
	if err := os.WriteFile(filepath.Join(wt2, "file_a.txt"), []byte("worker2 a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "file_b.txt"), []byte("worker2 b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 changes"); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	// Verify exactly file_a.txt and file_b.txt are conflicting
	// (file_c.txt was not modified by either worker post-base, unique1.txt is non-overlapping)
	got := make([]string, len(conflicts[0].ConflictFiles))
	copy(got, conflicts[0].ConflictFiles)
	sort.Strings(got)

	want := []string{"file_a.txt", "file_b.txt"}
	if len(got) != len(want) {
		t.Fatalf("ConflictFiles count = %d, want %d: got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ConflictFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

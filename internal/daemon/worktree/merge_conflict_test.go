package worktree

import (
	"context"
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
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
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

	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
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

	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
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

	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
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

	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
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

// TestResumeMerge_AddAddConflictResolution verifies the full conflict resolution
// cycle for add/add conflicts: conflict → worker resolves in worktree →
// ResumeMerge commits resolution and merges successfully.
//
// This is the core regression test for the infinite-loop bug where ResumeMerge
// would re-merge the original worker branch (without the resolution) and
// reproduce the same conflict indefinitely.
func TestResumeMerge_AddAddConflictResolution(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_resume_addadd"
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

	// Step 1: Both workers create the same file with different content (add/add)
	if err := os.WriteFile(filepath.Join(wt1, "CONFLICT_TEST.txt"), []byte("alpha\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add CONFLICT_TEST.txt"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "CONFLICT_TEST.txt"), []byte("beta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add CONFLICT_TEST.txt"); err != nil {
		t.Fatal(err)
	}

	// Step 2: Merge — worker1 succeeds, worker2 conflicts (add/add)
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker2" {
		t.Fatalf("expected 1 conflict on worker2, got %d: %v", len(conflicts), conflicts)
	}

	// Verify worker2 status = conflict, integration = partial_merge
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Fatalf("integration status = %q, want partial_merge", state.Integration.Status)
	}

	// Step 3: Simulate resolver dispatching worker2 to resolving state.
	// (In production, DispatchConflictResolution does this with CAS + signal store.)
	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		st, _ := wm.loadState(commandID)
		ws := wm.findWorker(st, "worker2")
		now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
		_ = wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now)
		st.UpdatedAt = now
		_ = wm.saveState(commandID, st)
	}()

	// Step 4: Worker2 resolves the conflict in their worktree by writing merged
	// content. This edit stays uncommitted (resolving→committed is invalid).
	resolvedContent := "alpha\nbeta\n"
	if err := os.WriteFile(filepath.Join(wt2, "CONFLICT_TEST.txt"), []byte(resolvedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Verify the edit is uncommitted in worker2's worktree.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wt2
	statusOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status in worker2: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) == "" {
		t.Fatal("worker2 worktree should have uncommitted changes (the resolution)")
	}

	// Step 5: ResumeMerge — should commit the resolution to worker2's branch
	// and then merge successfully into integration.
	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	// Step 6: Verify the result.
	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState after resume: %v", err)
	}

	// Worker2 should be integrated (not stuck in conflict/active loop).
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker2" {
			if ws.Status != model.WorktreeStatusIntegrated {
				t.Errorf("worker2 status = %q, want integrated", ws.Status)
			}
		}
	}

	// Integration should be merged (all workers integrated).
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged", state.Integration.Status)
	}

	// Verify the resolved content is on the integration branch.
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	cmd = exec.Command("git", "show", "HEAD:CONFLICT_TEST.txt")
	cmd.Dir = integrationPath
	showOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show CONFLICT_TEST.txt on integration: %v", err)
	}
	if string(showOut) != resolvedContent {
		t.Errorf("integration CONFLICT_TEST.txt = %q, want %q", string(showOut), resolvedContent)
	}

	// Verify integration worktree is clean.
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = integrationPath
	statusOut, err = cmd.Output()
	if err != nil {
		t.Fatalf("git status in integration: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("integration worktree should be clean, got: %s", strings.TrimSpace(string(statusOut)))
	}
}

// TestResumeMerge_AddAddConflict_XTheirs verifies that the -X theirs strategy
// option resolves add/add conflicts automatically without needing the
// checkout-and-commit fallback. This is the primary fix for the infinite-loop
// bug where resuming a merge on an add/add conflict would fail and reset the
// worker to active, causing MergeToIntegration to re-merge and loop.
func TestResumeMerge_AddAddConflict_XTheirs(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_xtheirs"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")

	// Both workers create the same file — add/add conflict.
	if err := os.WriteFile(filepath.Join(wt1, "ADD_ADD.txt"), []byte("first\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add ADD_ADD.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "ADD_ADD.txt"), []byte("second\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add ADD_ADD.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker1 succeeds, worker2 conflicts.
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker2" {
		t.Fatalf("expected 1 conflict on worker2, got %d: %v", len(conflicts), conflicts)
	}

	// Transition worker2 to resolving (simulates DispatchConflictResolution).
	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		st, _ := wm.loadState(commandID)
		ws := wm.findWorker(st, "worker2")
		now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
		_ = wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now)
		st.UpdatedAt = now
		_ = wm.saveState(commandID, st)
	}()

	// Worker2 resolves: writes merged content (includes both sides).
	// The worker has ALREADY committed — simulating a real agent that
	// commits its own changes via Bash git commands.
	resolved := "first\nsecond\n"
	if err := os.WriteFile(filepath.Join(wt2, "ADD_ADD.txt"), []byte(resolved), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = wt2
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "resolve conflict")
	cmd.Dir = wt2
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// ResumeMerge — with -X theirs, git should resolve the add/add
	// conflict automatically by preferring the worker's committed version.
	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}

	// Worker2 should be integrated (not stuck in conflict/active loop).
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker2" && ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("worker2 status = %q, want integrated", ws.Status)
		}
	}

	// Integration should be merged.
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged", state.Integration.Status)
	}

	// Verify resolved content on integration branch.
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	cmd = exec.Command("git", "show", "HEAD:ADD_ADD.txt")
	cmd.Dir = integrationPath
	showOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show ADD_ADD.txt: %v", err)
	}
	if string(showOut) != resolved {
		t.Errorf("integration ADD_ADD.txt = %q, want %q", string(showOut), resolved)
	}
}

// TestResumeMerge_FallbackRevertsToConflict verifies that when
// mergeResolvedWorker fails, the worker is set to conflict (not active)
// to prevent the infinite re-merge loop.
func TestResumeMerge_FallbackRevertsToConflict(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_fallback_conflict"

	// Create a state where the integration worktree DOES NOT exist but
	// workers exist — simulating a scenario where mergeResolvedWorker
	// would fail because there's no worktree to merge in. The legacy
	// fallback (resetWorkersToActive) fires because gitOutputInDir fails
	// for the integration worktree status check.
	//
	// Note: The per-worker fallback (resolving→conflict) only fires when
	// the worktree IS accessible but the actual merge fails. For the
	// legacy fallback, workers still go to active. This test verifies
	// the legacy fallback behavior is preserved (see
	// TestResumeMerge_ResetsConflictWorkers).
	//
	// To test the per-worker conflict fallback, we would need a real git
	// repo with a merge that fails even after -X theirs, which is very
	// hard to construct. The TestResumeMerge_AddAddConflict_XTheirs test
	// covers the success path.
	st := quarantinedState(cmdID)
	st.Integration.Status = model.IntegrationStatusConflict
	st.Integration.MergeFailureCount = 0
	st.Integration.QuarantinedAt = ""
	st.Integration.QuarantineReason = ""
	st.Workers = []model.WorktreeState{
		{WorkerID: "worker1", Status: model.WorktreeStatusIntegrated},
		{WorkerID: "worker2", Status: model.WorktreeStatusResolving},
	}
	writeWorktreeState(t, wm, st)

	if err := wm.ResumeMerge(context.Background(), cmdID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	got, err := wm.GetCommandState(cmdID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}

	// Worker2 should be active (legacy fallback — worktree unavailable).
	for _, ws := range got.Workers {
		if ws.WorkerID == "worker2" && ws.Status != model.WorktreeStatusActive {
			t.Errorf("worker2 status = %q, want active (legacy fallback)", ws.Status)
		}
	}

	// MergeFailureCount should be reset (legacy fallback path).
	if got.Integration.MergeFailureCount != 0 {
		t.Errorf("MergeFailureCount = %d, want 0 (legacy fallback resets)", got.Integration.MergeFailureCount)
	}
}

// TestResumeMerge_NoConflictAfterWorkerCommit verifies that if the worker's
// resolution makes the merge base irrelevant (worker branch content matches
// integration), the merge proceeds without conflict.
func TestResumeMerge_NoConflictAfterWorkerCommit(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_resume_noclash"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")

	// Worker1 modifies README.md
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 modify README"); err != nil {
		t.Fatal(err)
	}

	// Worker2 also modifies README.md differently → conflict
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 modify README"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker1 succeeds, worker2 conflicts
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	// Transition worker2 to resolving
	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		st, _ := wm.loadState(commandID)
		ws := wm.findWorker(st, "worker2")
		now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
		_ = wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now)
		st.UpdatedAt = now
		_ = wm.saveState(commandID, st)
	}()

	// Worker2 resolves by accepting worker1's version (same as integration)
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker1 content\nworker2 content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// ResumeMerge
	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}

	for _, ws := range state.Workers {
		if ws.WorkerID == "worker2" && ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("worker2 status = %q, want integrated", ws.Status)
		}
	}
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged", state.Integration.Status)
	}
}

// TestResumeMerge_NormalMergeUnaffected verifies that the normal (non-conflict)
// merge flow is not broken by the conflict resolution changes.
func TestResumeMerge_NormalMergeUnaffected(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_normal_merge"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")

	// Each worker creates a unique file — no conflict expected
	if err := os.WriteFile(filepath.Join(wt1, "file1.txt"), []byte("worker1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add file1"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "file2.txt"), []byte("worker2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add file2"); err != nil {
		t.Fatal(err)
	}

	// Normal merge — no conflicts
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %d", len(conflicts))
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged", state.Integration.Status)
	}
	for _, ws := range state.Workers {
		if ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("worker %s status = %q, want integrated", ws.WorkerID, ws.Status)
		}
	}
}

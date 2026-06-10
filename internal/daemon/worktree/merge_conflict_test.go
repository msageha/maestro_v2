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
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestMergeConflict_Detection verifies that MergeToIntegration correctly detects
// a conflict when two workers modify the same file with different content.
// Verifies: conflict count, correct worker identified, integration worktree clean after abort.
func TestMergeConflict_Detection(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
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
	projectRoot := testutil.InitTestGitRepo(t)
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
	projectRoot := testutil.InitTestGitRepo(t)
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

	// Verify the refs are commit SHAs (not blob SHAs): "git show <sha>:<file>"
	// must succeed. If the refs were blob SHAs, this command would fail because
	// blob SHAs cannot be used with the <sha>:<path> syntax.
	for _, ref := range []struct {
		name string
		sha  string
	}{
		{"OursRef", mc.OursRef},
		{"TheirsRef", mc.TheirsRef},
		{"BaseRef", mc.BaseRef},
	} {
		cmd := exec.Command("git", "show", ref.sha+":README.md")
		cmd.Dir = projectRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s=%s is not a commit SHA: git show %s:README.md failed: %v\n%s",
				ref.name, ref.sha, ref.sha, err, out)
		}
	}
}

// TestMergeConflict_BinaryFile verifies that binary file conflicts correctly
// extract non-empty base/ours/theirs refs (W5: binary files must not produce
// empty refs when the base version exists).
func TestMergeConflict_BinaryFile(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
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
	projectRoot := testutil.InitTestGitRepo(t)
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
	projectRoot := testutil.InitTestGitRepo(t)
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
	projectRoot := testutil.InitTestGitRepo(t)
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

	// Worker2 resolves: writes merged content (includes both sides) into
	// their worktree as uncommitted edits. This matches production worker
	// policy (templates/instructions/worker.md: workers must not run
	// `git add` / `git commit` themselves; the daemon commits via
	// commitResolvedWorkerChanges from inside ResumeMerge).
	//
	// History note: this test previously self-committed via shell git
	// commands. That path was incompatible with the in-flight resolution
	// race guard added in tryMergeWorker (a clean worktree on a worker that
	// arrived already in resolving status now means "resolution task is
	// still in flight" and is intentionally skipped). The non-self-commit
	// shape exercises the same `-X theirs` strategy this test cares about
	// because commitResolvedWorkerChanges produces a fresh commit before
	// mergeResolvedWorker runs.
	resolved := "first\nsecond\n"
	if err := os.WriteFile(filepath.Join(wt2, "ADD_ADD.txt"), []byte(resolved), 0644); err != nil {
		t.Fatal(err)
	}

	// ResumeMerge — daemon commits worker2's uncommitted resolution and
	// then merges the now-fresh worker branch into integration with
	// -X theirs to handle the add/add conflict.
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
	cmd := exec.Command("git", "show", "HEAD:ADD_ADD.txt")
	cmd.Dir = integrationPath
	showOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show ADD_ADD.txt: %v", err)
	}
	if string(showOut) != resolved {
		t.Errorf("integration ADD_ADD.txt = %q, want %q", string(showOut), resolved)
	}
}

// TestResumeMerge_SkipsResolvingWithoutEdits is the regression test for the
// race where a non-AutoRecoverAfterResolution caller fires ResumeMerge while
// R7's __conflict_resolution task is still in flight. Production behaviour
// (templates/instructions/worker.md) is that workers leave conflict
// resolutions as uncommitted edits, so an arrived-resolving worker with a
// clean worktree means the resolution task has not produced any result yet.
// Without the gate inside tryMergeWorker, mergeResolvedWorker would commit
// nothing in commitResolvedWorkerChanges, fall through with the unchanged
// worker branch, and merge the *pre-resolution* content via -X theirs —
// silently overwriting the resolution. The gate keeps the worker in
// resolving so the worker's eventual result_write triggers the proper
// AutoRecoverAfterResolution path.
func TestResumeMerge_SkipsResolvingWithoutEdits(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_inflight_skip"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")

	if err := os.WriteFile(filepath.Join(wt1, "INFLIGHT.txt"), []byte("worker1 wins\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add INFLIGHT.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "INFLIGHT.txt"), []byte("worker2 wins\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add INFLIGHT.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	// Simulate R7 dispatching the resolution task: worker2 transitions to
	// resolving, but the actual resolution task has not yet completed —
	// worker2's worktree therefore has no uncommitted edits.
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

	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState after resume: %v", err)
	}

	// The race guard must keep worker2 in resolving rather than promoting
	// it to integrated using the unchanged (pre-resolution) branch.
	for _, ws := range state.Workers {
		if ws.WorkerID != "worker2" {
			continue
		}
		if ws.Status != model.WorktreeStatusResolving {
			t.Errorf("worker2 status = %q, want resolving (in-flight resolution must not be merged)", ws.Status)
		}
	}

	// Integration must not flip to merged on a half-baked resolution.
	if state.Integration.Status == model.IntegrationStatusMerged {
		t.Errorf("integration status = merged but worker2's resolution was still in flight")
	}

	// Worker2 then completes the resolution by leaving merged content as
	// uncommitted edits (production worker policy). A subsequent ResumeMerge
	// — same call shape, but now the worktree has real edits — must drive
	// the worker to integrated.
	resolved := "worker1 wins\nworker2 wins\n"
	if err := os.WriteFile(filepath.Join(wt2, "INFLIGHT.txt"), []byte(resolved), 0644); err != nil {
		t.Fatal(err)
	}

	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge after edits: %v", err)
	}

	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState after second resume: %v", err)
	}
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker2" && ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("worker2 status = %q, want integrated after resolution edits", ws.Status)
		}
	}
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged after resolution edits", state.Integration.Status)
	}

	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	cmd := exec.Command("git", "show", "HEAD:INFLIGHT.txt")
	cmd.Dir = integrationPath
	showOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show INFLIGHT.txt: %v", err)
	}
	if string(showOut) != resolved {
		t.Errorf("integration INFLIGHT.txt = %q, want %q", string(showOut), resolved)
	}
}

// TestResumeMerge_ResolvingSelfCommit_ProgressesToIntegrated covers the
// non-Claude Worker path where the Worker (codex / gemini) self-commits the
// conflict resolution before the daemon's commitResolvedWorkerChanges runs.
// The original deferred guard treated "clean worktree on a resolving worker"
// as "resolution task still in flight" — but with a self-commit the worktree
// is clean *because* the resolution is already on the worker branch HEAD.
// Without distinguishing these two cases the worker would be pinned at
// resolving forever (deferred → no edits to commit → deferred again).
//
// The fix compares HEAD against ws.ConflictBranchHead (the worker branch
// SHA snapshotted at conflict detection): when HEAD has advanced past it,
// treat the worker as having completed the resolution and proceed with
// the merge.
func TestResumeMerge_ResolvingSelfCommit_ProgressesToIntegrated(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_self_commit"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")

	if err := os.WriteFile(filepath.Join(wt1, "SELFCOMMIT.txt"), []byte("worker1 wins\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add SELFCOMMIT.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "SELFCOMMIT.txt"), []byte("worker2 wins\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add SELFCOMMIT.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	// Simulate R7 dispatching the resolution task.
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

	// Worker self-commits the resolution directly — the worktree is left
	// clean by the time ResumeMerge runs. This is the codex / gemini
	// behaviour we have to support: those agents do not run under the
	// Claude policy hook so they can issue `git commit` themselves.
	resolved := "worker1 wins\nworker2 wins\n"
	if err := os.WriteFile(filepath.Join(wt2, "SELFCOMMIT.txt"), []byte(resolved), 0644); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", "SELFCOMMIT.txt")
	addCmd.Dir = wt2
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("worker self-commit (git add): %v: %s", err, out)
	}
	commitCmd := exec.Command("git",
		"-c", "user.email=worker2@test", "-c", "user.name=worker2",
		"commit", "-m", "worker2 self-commit resolution")
	commitCmd.Dir = wt2
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("worker self-commit (git commit): %v: %s", err, out)
	}

	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	for _, ws := range state.Workers {
		if ws.WorkerID != "worker2" {
			continue
		}
		if ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("worker2 status = %q, want integrated (self-commit must unblock the merge)", ws.Status)
		}
	}
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want merged", state.Integration.Status)
	}

	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	showCmd := exec.Command("git", "show", "HEAD:SELFCOMMIT.txt")
	showCmd.Dir = integrationPath
	showOut, err := showCmd.Output()
	if err != nil {
		t.Fatalf("git show SELFCOMMIT.txt: %v", err)
	}
	if string(showOut) != resolved {
		t.Errorf("integration SELFCOMMIT.txt = %q, want %q", string(showOut), resolved)
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
	projectRoot := testutil.InitTestGitRepo(t)
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

// TestResumeMerge_BlankResolutionDoesNotPromoteToMerged: when worker2 is in
// Conflict and the "resolution" task did NOT touch the conflicting file,
// commitResolvedWorkerChanges has nothing staged (no dirty changes at all)
// so the conflict on SHARED.txt persists and ResumeMerge must not falsely
// promote the integration to Merged.
//
// The previous variant of this test relied on the orchestrator-level
// sensitive-file filter discarding *.key files. That filter is no longer
// part of the orchestrator (worker output is captured verbatim), so the
// content-mismatch scenario must be exercised by leaving the worktree
// genuinely clean rather than relying on a path-based exclusion.
func TestResumeMerge_BlankResolutionDoesNotPromoteToMerged(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_resume_blank_resolution"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1: %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2: %v", err)
	}

	// Both workers add the same file with different content → add/add conflict.
	if err := os.WriteFile(filepath.Join(wt1, "SHARED.txt"), []byte("alpha\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add SHARED.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "SHARED.txt"), []byte("beta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add SHARED.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker1 integrates, worker2 conflicts.
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker2" {
		t.Fatalf("expected 1 conflict on worker2, got %d: %v", len(conflicts), conflicts)
	}

	// Simulate resolver dispatch → worker2 goes to Resolving.
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

	// Worker2's "resolution" task left the worktree completely unchanged —
	// SHARED.txt remains in conflict-marker state on disk. ResumeMerge must
	// detect that the underlying conflict is not resolved.

	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}

	if state.Integration.Status == model.IntegrationStatusMerged {
		t.Errorf("integration status = merged (BAD: blank resolution must not promote to merged)")
	}
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker2" && ws.Status == model.WorktreeStatusIntegrated {
			t.Errorf("worker2 status = integrated (BAD: blank resolution must not commit)")
		}
	}
}

// TestResumeMerge_NormalMergeUnaffected verifies that the normal (non-conflict)
// merge flow is not broken by the conflict resolution changes.
func TestResumeMerge_NormalMergeUnaffected(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
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

// TestResumeMerge_StaleResolutionDoesNotClobberIntegration reproduces the
// parallel-conflict lost-update scenario found in the 2026-06-10 E2E run:
//
//  1. worker1 merges cleanly into integration.
//  2. worker2 and worker3 both conflict against the SAME integration HEAD and
//     resolve in parallel — each resolution incorporates only the integration
//     content visible at the conflict snapshot.
//  3. ResumeMerge merges worker2's resolution first (integration advances),
//     then worker3's resolution. Before the fix, worker3's merge ran with
//     -X theirs and silently dropped worker2's resolved content.
//
// With the lost-update guard, worker3's stale resolution must NOT be merged:
// the worker is reset to active so the standard merge pipeline re-detects the
// conflict against the current integration HEAD (fresh ours ref), and the
// second resolution round converges with no content loss.
func TestResumeMerge_StaleResolutionDoesNotClobberIntegration(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_stale_resolution"
	workers := []string{"worker1", "worker2", "worker3"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")
	wt3, _ := wm.GetWorkerPath(commandID, "worker3")

	// All three workers create the same file with different content so that
	// worker1 merges cleanly and worker2/worker3 conflict against the same
	// integration HEAD (the one containing worker1's content).
	if err := os.WriteFile(filepath.Join(wt1, "FEATURE.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 adds one"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "FEATURE.txt"), []byte("two\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 adds two"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt3, "FEATURE.txt"), []byte("three\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker3", "worker3 adds three"); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts (worker2, worker3), got %d: %v", len(conflicts), conflicts)
	}

	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	headOut, err := exec.Command("git", "-C", integrationPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse integration HEAD: %v", err)
	}
	conflictSnapshot := strings.TrimSpace(string(headOut))

	// Both conflicted workers must have pinned the integration HEAD they were
	// asked to resolve against.
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker1" {
			continue
		}
		if ws.Status != model.WorktreeStatusConflict {
			t.Fatalf("worker %s status = %q, want conflict", ws.WorkerID, ws.Status)
		}
		if ws.ConflictIntegrationHead != conflictSnapshot {
			t.Fatalf("worker %s ConflictIntegrationHead = %q, want %q",
				ws.WorkerID, ws.ConflictIntegrationHead, conflictSnapshot)
		}
	}

	// Simulate the resolver dispatching both workers in parallel, each
	// resolving against the SAME conflict snapshot (integration content "one").
	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		st, _ := wm.loadState(commandID)
		now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
		for _, wid := range []string{"worker2", "worker3"} {
			ws := wm.findWorker(st, wid)
			_ = wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now)
		}
		st.UpdatedAt = now
		_ = wm.saveState(commandID, st)
	}()

	// worker2's resolution: integration content at snapshot + own content.
	if err := os.WriteFile(filepath.Join(wt2, "FEATURE.txt"), []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// worker3's resolution: ALSO computed against the snapshot — it has never
	// seen worker2's content. Pre-fix, -X theirs made this the final content.
	if err := os.WriteFile(filepath.Join(wt3, "FEATURE.txt"), []byte("one\nthree\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge: %v", err)
	}

	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState after resume: %v", err)
	}
	var w2, w3 *model.WorktreeState
	for i := range state.Workers {
		switch state.Workers[i].WorkerID {
		case "worker2":
			w2 = &state.Workers[i]
		case "worker3":
			w3 = &state.Workers[i]
		}
	}
	if w2.Status != model.WorktreeStatusIntegrated {
		t.Errorf("worker2 status = %q, want integrated", w2.Status)
	}
	// worker3's resolution was stale — it must NOT be integrated via -X theirs.
	if w3.Status != model.WorktreeStatusActive {
		t.Errorf("worker3 status = %q, want active (stale resolution reset)", w3.Status)
	}

	// worker2's resolved content must survive on integration.
	showOut, err := exec.Command("git", "-C", integrationPath, "show", "HEAD:FEATURE.txt").Output()
	if err != nil {
		t.Fatalf("git show FEATURE.txt: %v", err)
	}
	if got := string(showOut); got != "one\ntwo\n" {
		t.Fatalf("integration FEATURE.txt after stale guard = %q, want %q (worker2 content clobbered)", got, "one\ntwo\n")
	}

	// Round 2: the standard merge pipeline re-merges the active worker3 (its
	// branch carries the committed stale resolution) and must re-detect the
	// conflict against the CURRENT integration HEAD, repinning the snapshot.
	conflicts, err = wm.MergeToIntegration(context.Background(), commandID, []string{"worker3"}, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration round2: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker3" {
		t.Fatalf("round2: expected fresh conflict on worker3, got %v", conflicts)
	}

	newHeadOut, err := exec.Command("git", "-C", integrationPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse integration HEAD round2: %v", err)
	}
	newSnapshot := strings.TrimSpace(string(newHeadOut))
	if newSnapshot == conflictSnapshot {
		t.Fatal("integration HEAD should have advanced past the original conflict snapshot")
	}

	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState round2: %v", err)
	}
	w3 = wm.findWorker(state, "worker3")
	if w3.ConflictIntegrationHead != newSnapshot {
		t.Fatalf("round2: worker3 ConflictIntegrationHead = %q, want refreshed %q",
			w3.ConflictIntegrationHead, newSnapshot)
	}

	// Round-2 resolution now sees the up-to-date integration content.
	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		st, _ := wm.loadState(commandID)
		ws := wm.findWorker(st, "worker3")
		now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
		_ = wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now)
		st.UpdatedAt = now
		_ = wm.saveState(commandID, st)
	}()
	if err := os.WriteFile(filepath.Join(wt3, "FEATURE.txt"), []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.ResumeMerge(context.Background(), commandID); err != nil {
		t.Fatalf("ResumeMerge round2: %v", err)
	}

	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState final: %v", err)
	}
	w3 = wm.findWorker(state, "worker3")
	if w3.Status != model.WorktreeStatusIntegrated {
		t.Errorf("final: worker3 status = %q, want integrated", w3.Status)
	}
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("final: integration status = %q, want merged", state.Integration.Status)
	}
	showOut, err = exec.Command("git", "-C", integrationPath, "show", "HEAD:FEATURE.txt").Output()
	if err != nil {
		t.Fatalf("git show FEATURE.txt final: %v", err)
	}
	if got := string(showOut); got != "one\ntwo\nthree\n" {
		t.Fatalf("final integration FEATURE.txt = %q, want %q (lost update)", got, "one\ntwo\nthree\n")
	}
}

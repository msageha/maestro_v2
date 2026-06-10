package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestPublishToBase_RefAdvancedSyncFailure_QuarantinesWithAccurateReason pins
// the double-failure path: the base ref CAS already advanced to the publish
// merge, the projectRoot working-tree sync (reset --hard) fails, and the CAS
// rollback of the ref also fails. The publish must transition straight to
// quarantine with the dedicated publish_ref_advanced_root_sync_failed reason
// — NOT enter the generic retry-with-backoff path, which would re-run publish
// against the half-synced root, trip the dirty-root guard, and quarantine
// with a misleading "uncommitted changes" message.
func TestPublishToBase_RefAdvancedSyncFailure_QuarantinesWithAccurateReason(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	gitOut := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = projectRoot
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}

	baseBranch := gitOut("branch", "--show-current")
	wm.config.BaseBranch = baseBranch
	preSHA := gitOut("rev-parse", baseBranch)

	const commandID = "cmd_rollback_fail"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "published.txt"), []byte("published"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add published.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatal(err)
	}

	// Simulate the double failure: the reset hook moves the base ref away
	// from mergeSHA (so the CAS rollback's old-value check fails) and then
	// reports a sync failure.
	wm.testPublishResetHook = func() error {
		cmd := exec.Command("git", "update-ref", "refs/heads/"+baseBranch, preSHA)
		cmd.Dir = projectRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("test hook update-ref: %v (%s)", err, out)
		}
		return errors.New("simulated reset --hard failure")
	}

	err = wm.PublishToBase(commandID, "")
	if err == nil {
		t.Fatal("PublishToBase should fail when sync and rollback both fail")
	}
	if !errors.Is(err, errPublishRefAdvancedSyncFailed) {
		t.Errorf("error should wrap errPublishRefAdvancedSyncFailed, got: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusQuarantined {
		t.Errorf("integration status = %q, want %q (terminal, not retry-with-backoff)",
			state.Integration.Status, model.IntegrationStatusQuarantined)
	}
	if !strings.Contains(state.Integration.QuarantineReason, "publish_ref_advanced_root_sync_failed") {
		t.Errorf("QuarantineReason = %q, want it to name publish_ref_advanced_root_sync_failed",
			state.Integration.QuarantineReason)
	}
	if state.Integration.NextPublishRetryAt != "" {
		t.Errorf("NextPublishRetryAt = %q, want empty (no automatic retry scheduling)",
			state.Integration.NextPublishRetryAt)
	}
}

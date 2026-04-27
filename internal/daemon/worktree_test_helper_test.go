package daemon

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TODO(DRY): initTestGitRepo, newTestWorktreeManager are duplicated.
// Duplicated in: worktree/worktree_test_helper_test.go (same helpers, different package)
// Target: internal/testutil/worktree.go (shared across packages)
// Trigger: promote to testutil when WorktreeManager no longer requires the
// daemon-internal config wiring inlined below (i.e. once
// `internal/daemon/worktree` exposes a config builder reusable from both
// daemon-level and worktree-level tests). Until then the daemon-level
// helper has slightly different defaults from the worktree-package twin
// and merging them prematurely loses test fidelity.

// initTestGitRepo delegates to testutil.InitTestGitRepo.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	return testutil.InitTestGitRepo(t)
}

// newTestWorktreeManager creates a WorktreeManager for testing.
// Duplicated from worktree test package for daemon-level tests.
func newTestWorktreeManager(t *testing.T, projectRoot string) *WorktreeManager {
	t.Helper()
	maestroDir := filepath.Join(projectRoot, ".maestro")
	if err := os.MkdirAll(maestroDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := model.WorktreeConfig{
		Enabled:          true,
		BaseBranch:       "main",
		PathPrefix:       ".maestro/worktrees",
		AutoCommit:       true,
		AutoMerge:        true,
		MergeStrategy:    "ort",
		CleanupOnSuccess: true,
		CleanupOnFailure: false,
		GC: model.WorktreeGCConfig{
			Enabled:      true,
			TTLHours:     ptr.Int(24),
			MaxWorktrees: ptr.Int(32),
		},
		CommitPolicy: model.CommitPolicyConfig{},
	}

	logger := log.New(os.Stderr, "", 0)
	return NewWorktreeManager(maestroDir, cfg, logger, LogLevelError)
}

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

// TODO(DRY): initTestGitRepo and newTestWorktreeManager are duplicated
// across daemon and worktree test packages. Consider consolidating into
// a shared internal/testutil/worktree.go helper once the test structure stabilizes.

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

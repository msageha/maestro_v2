package dispatch

import (
	"bytes"
	"log"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// refreshTrackingResolver records whether RefreshWorkerWorktreeToIntegrationHead
// is called during dispatch, so the test can prove that repair tasks bypass
// it while normal tasks still trigger it.
type refreshTrackingResolver struct {
	stubResolver
	wtPath        string
	refreshCalled int
	ensureCalled  int
}

func (r *refreshTrackingResolver) GetWorkerPath(string, string) (string, error) {
	if r.wtPath == "" {
		return "/tmp/test-worktree", nil
	}
	return r.wtPath, nil
}

func (r *refreshTrackingResolver) EnsureWorkerWorktree(string, string) error {
	r.ensureCalled++
	return nil
}

func (r *refreshTrackingResolver) RefreshWorkerWorktreeToIntegrationHead(string, string) error {
	r.refreshCalled++
	return nil
}

// TestResolveTaskWorkingDir_SkipsRefreshForRepairTask pins the invariant:
// when a verify_repair (or planner-side retry) task is dispatched, the worker
// worktree still carries the original task's uncommitted edits because Phase B
// auto-commit only runs after verify succeeds. Calling
// RefreshWorkerWorktreeToIntegrationHead would abort with "uncommitted changes;
// refresh aborted" and dead-letter the repair, so the dispatcher must skip
// the refresh for OperationTypeRepair tasks.
func TestResolveTaskWorkingDir_SkipsRefreshForRepairTask(t *testing.T) {
	resolver := &refreshTrackingResolver{}

	disp := New("/tmp/maestro", model.Config{}, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug, nil, core.RealClock{})
	disp.SetWorktreeManager(resolver)

	repairTask := &model.Task{
		ID:            "task_repair_1",
		CommandID:     "cmd_xxx",
		OperationType: model.OperationTypeRepair,
	}

	wd, err := disp.resolveTaskWorkingDir(repairTask, "worker1", false)
	if err != nil {
		t.Fatalf("unexpected error for repair task: %v", err)
	}
	if wd == "" {
		t.Fatal("expected non-empty working dir for repair task")
	}
	if resolver.refreshCalled != 0 {
		t.Errorf("expected refresh to be skipped for repair task, but it was called %d times", resolver.refreshCalled)
	}
}

// TestResolveTaskWorkingDir_RefreshesForNormalTask is the inverse contract:
// normal (non-repair) tasks must still be refreshed so a worker re-used
// across phases sees sibling-worker commits already on integration. This
// guards against the "skip-too-eagerly" failure mode where the refresh fix
// from the repair path would inadvertently disable refresh entirely.
func TestResolveTaskWorkingDir_RefreshesForNormalTask(t *testing.T) {
	resolver := &refreshTrackingResolver{}

	disp := New("/tmp/maestro", model.Config{}, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug, nil, core.RealClock{})
	disp.SetWorktreeManager(resolver)

	normalTask := &model.Task{
		ID:        "task_normal_1",
		CommandID: "cmd_xxx",
		// OperationType intentionally empty (normal task).
	}

	if _, err := disp.resolveTaskWorkingDir(normalTask, "worker1", false); err != nil {
		t.Fatalf("unexpected error for normal task: %v", err)
	}
	if resolver.refreshCalled != 1 {
		t.Errorf("expected exactly one refresh for normal task, got %d", resolver.refreshCalled)
	}
}

// TestResolveTaskWorkingDir_RefreshesForVerifyTask covers the verify-task path
// alongside the normal-task case: verify tasks (OperationTypeVerify) are
// daemon-injected read-only sweeps that must observe the integration HEAD
// after merges. Skipping refresh would let them read stale code, defeating
// their purpose.
func TestResolveTaskWorkingDir_RefreshesForVerifyTask(t *testing.T) {
	resolver := &refreshTrackingResolver{}

	disp := New("/tmp/maestro", model.Config{}, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug, nil, core.RealClock{})
	disp.SetWorktreeManager(resolver)

	verifyTask := &model.Task{
		ID:            "task_verify_1",
		CommandID:     "cmd_xxx",
		OperationType: model.OperationTypeVerify,
	}

	if _, err := disp.resolveTaskWorkingDir(verifyTask, "worker1", false); err != nil {
		t.Fatalf("unexpected error for verify task: %v", err)
	}
	if resolver.refreshCalled != 1 {
		t.Errorf("expected exactly one refresh for verify task, got %d", resolver.refreshCalled)
	}
}

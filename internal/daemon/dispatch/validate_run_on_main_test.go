package dispatch

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

type stubIntegrationStatus struct {
	status model.IntegrationStatus
	err    error
}

func (s stubIntegrationStatus) GetIntegrationStatus(string) (model.IntegrationStatus, error) {
	return s.status, s.err
}

func claudeWorkerCfg() model.WorkerConfig {
	return model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
}

func TestValidateRunOnMainPreflight_NonRunOnMainTask_NoOp(t *testing.T) {
	task := &model.Task{ID: "t1", CommandID: "cmd1"}
	if err := validateRunOnMainPreflight(task, "worker1", model.WorkerConfig{DefaultModel: "codex"}, nil); err != nil {
		t.Errorf("non-run_on_main task must pass pre-flight unconditionally: %v", err)
	}
}

func TestValidateRunOnMainPreflight_NonClaudeWorker_Rejected(t *testing.T) {
	task := &model.Task{ID: "t1", CommandID: "cmd1", RunOnMain: true}
	cfg := model.WorkerConfig{Count: 2, DefaultModel: "sonnet", Models: map[string]string{"worker1": "codex"}}

	err := validateRunOnMainPreflight(task, "worker1", cfg, stubIntegrationStatus{status: model.IntegrationStatusPublished})
	if err == nil {
		t.Fatal("expected rejection for run_on_main task on a codex worker")
	}
	if !errors.Is(err, ErrRunOnMainPreflightRejected) {
		t.Errorf("error must wrap ErrRunOnMainPreflightRejected for non-retryable termination, got: %v", err)
	}
}

func TestValidateRunOnMainPreflight_PrePublishIntegration_Rejected(t *testing.T) {
	task := &model.Task{ID: "t1", CommandID: "cmd1", RunOnMain: true}

	// `created` is rejected too: worktree state is only born when normal
	// worker tasks dispatch (EnsureWorkerWorktree), so a state file at
	// `created` means in-flight worker output that has not merged yet —
	// running the verification now would read stale main.
	for _, status := range []model.IntegrationStatus{
		model.IntegrationStatusCreated,
		model.IntegrationStatusMerging,
		model.IntegrationStatusMerged,
		model.IntegrationStatusConflict,
		model.IntegrationStatusPartialMerge,
		model.IntegrationStatusPublishing,
		model.IntegrationStatusPublishFailed,
		model.IntegrationStatusQuarantined,
		model.IntegrationStatusFailed,
	} {
		err := validateRunOnMainPreflight(task, "worker1", claudeWorkerCfg(), stubIntegrationStatus{status: status})
		if err == nil {
			t.Errorf("status=%s: expected rejection before publish", status)
			continue
		}
		if !errors.Is(err, ErrRunOnMainPreflightRejected) {
			t.Errorf("status=%s: error must wrap ErrRunOnMainPreflightRejected, got: %v", status, err)
		}
	}
}

func TestValidateRunOnMainPreflight_AllowedStates(t *testing.T) {
	task := &model.Task{ID: "t1", CommandID: "cmd1", RunOnMain: true}

	cases := []struct {
		name string
		wm   integrationStatusReader
	}{
		{"published", stubIntegrationStatus{status: model.IntegrationStatusPublished}},
		// run_on_main-only verification commands never create worktree
		// state (EnsureWorkerWorktree only runs for normal worker tasks),
		// so the absent-file path is what those commands exercise.
		{"state file absent", stubIntegrationStatus{err: fmt.Errorf("read: %w", os.ErrNotExist)}},
		{"worktree mode disabled", nil},
	}
	for _, tc := range cases {
		if err := validateRunOnMainPreflight(task, "worker1", claudeWorkerCfg(), tc.wm); err != nil {
			t.Errorf("%s: expected pre-flight pass, got: %v", tc.name, err)
		}
	}
}

func TestValidateRunOnMainPreflight_TransientReadError_NotTerminal(t *testing.T) {
	task := &model.Task{ID: "t1", CommandID: "cmd1", RunOnMain: true}

	err := validateRunOnMainPreflight(task, "worker1", claudeWorkerCfg(), stubIntegrationStatus{err: errors.New("disk io")})
	if err == nil {
		t.Fatal("expected error for transient status read failure")
	}
	if errors.Is(err, ErrRunOnMainPreflightRejected) {
		t.Error("transient read failures must NOT wrap the terminal sentinel — the lease-expiry path should retry instead of dead-ending the task")
	}
}

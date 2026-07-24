package model

import "testing"

func TestTaskResumeEligible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		task Task
		want bool
	}{
		{name: "default_worker_worktree_task", task: Task{}, want: true},
		{name: "run_on_main_read_only", task: Task{RunOnMain: true}, want: true},
		{name: "run_on_integration_denied", task: Task{RunOnIntegration: true}, want: false},
		{name: "hint_deny_overrides_default", task: Task{ResumeHint: ResumeHintDeny}, want: false},
		{name: "hint_allow_overrides_run_on_integration", task: Task{RunOnIntegration: true, ResumeHint: ResumeHintAllow}, want: true},
		// PR #56 review finding #21: unknown non-empty hints fail closed —
		// a hand-edited or future-version YAML value we cannot interpret
		// must not opt the task into in-place resume.
		{name: "unknown_hint_fails_closed", task: Task{ResumeHint: "maybe"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.task.ResumeEligible(); got != tt.want {
				t.Errorf("ResumeEligible() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRetryConfig_ProgressInterruptAndResumeDefaults(t *testing.T) {
	t.Parallel()
	var r RetryConfig
	if got := r.EffectiveTaskProgressInterrupts(); got != DefaultTaskProgressInterrupts {
		t.Errorf("EffectiveTaskProgressInterrupts() = %d, want %d", got, DefaultTaskProgressInterrupts)
	}
	if got := r.EffectiveTaskResume(); got != DefaultTaskResume {
		t.Errorf("EffectiveTaskResume() = %d, want %d", got, DefaultTaskResume)
	}
	zero := 0
	r.TaskProgressInterrupts = &zero
	r.TaskResume = &zero
	if got := r.EffectiveTaskProgressInterrupts(); got != 0 {
		t.Errorf("explicit 0 must disable the exemption; got %d", got)
	}
	if got := r.EffectiveTaskResume(); got != 0 {
		t.Errorf("explicit 0 must disable resume; got %d", got)
	}
}

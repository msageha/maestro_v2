package reconcile

import (
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// PR #56 review finding #2: R1's rebuilt retry task must not inherit the
// per-instance progress/resume ledgers. The clone restarts at LeaseEpoch 0,
// so an inherited LastProgressEpoch==1 would classify its first epoch as
// "had progress" even when it wedges without output. ResumeHint is policy
// and must be preserved.
func TestR1BuildRetryTask_ResetsProgressAndResumeLedger(t *testing.T) {
	t.Parallel()
	original := &model.Task{
		ID:                   "task_orig",
		CommandID:            "cmd_orig",
		Status:               model.StatusFailed,
		Attempts:             5,
		LeaseEpoch:           1,
		LastProgressEpoch:    1,
		AttemptsChargedEpoch: 1,
		ProgressInterrupts:   6,
		ResumeAttempts:       3,
		ResumeRequested:      true,
		ResumeHint:           model.ResumeHintDeny,
	}
	clock := &testutil.FakeClock{NowValue: time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)}
	clone := r1BuildRetryTask(original, "task_rebuild", clock)

	if clone.LastProgressEpoch != 0 || clone.AttemptsChargedEpoch != 0 ||
		clone.ProgressInterrupts != 0 || clone.ResumeAttempts != 0 || clone.ResumeRequested {
		t.Errorf("ledger fields not reset: last_progress_epoch=%d attempts_charged_epoch=%d progress_interrupts=%d resume_attempts=%d resume_requested=%t",
			clone.LastProgressEpoch, clone.AttemptsChargedEpoch, clone.ProgressInterrupts, clone.ResumeAttempts, clone.ResumeRequested)
	}
	if clone.ResumeHint != model.ResumeHintDeny {
		t.Errorf("ResumeHint = %q, want %q (policy must be inherited)", clone.ResumeHint, model.ResumeHintDeny)
	}
	if clone.Attempts != 0 || clone.LeaseEpoch != 0 {
		t.Errorf("Attempts/LeaseEpoch not reset: attempts=%d lease_epoch=%d", clone.Attempts, clone.LeaseEpoch)
	}
}

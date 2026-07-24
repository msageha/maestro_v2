package daemon

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// newProgressInterruptQH builds a QueueHandler with a fixed clock suitable
// for exercising applyTaskBusyCheckResult's hang-release accounting.
func newProgressInterruptQH(t *testing.T, now time.Time, retry model.RetryConfig) *QueueHandler {
	t.Helper()
	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents: model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{
			DispatchLeaseSec: 300,
			ScanIntervalSec:  10,
			MaxInProgressMin: ptr.Int(60),
		},
		Queue: model.QueueConfig{PriorityAgingSec: 60},
		Retry: retry,
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}
	return qh
}

// hangReleaseFixture builds an in_progress task with an expired lease and the
// matching idle busy-check result so applyTaskBusyCheckResult takes the
// hang-release path (in_progress → pending, !Busy, !Undecided).
func hangReleaseFixture(now time.Time, task model.Task) (busyCheckResult, map[string]*taskQueueEntry) {
	expiresAt := now.Add(-1 * time.Second).Format(time.RFC3339)
	updatedAt := now.Add(-10 * time.Minute).Format(time.RFC3339)

	task.Status = model.StatusInProgress
	owner := "daemon"
	task.LeaseOwner = &owner
	task.LeaseExpiresAt = &expiresAt
	task.UpdatedAt = updatedAt

	queueFile := "queue/worker1.yaml"
	taskQueues := map[string]*taskQueueEntry{
		queueFile: {Queue: model.TaskQueue{Tasks: []model.Task{task}}, Path: queueFile},
	}
	bc := busyCheckResult{
		Item: busyCheckItem{
			Kind:      "task",
			EntryID:   task.ID,
			AgentID:   "worker1",
			Epoch:     task.LeaseEpoch,
			QueueFile: queueFile,
			UpdatedAt: updatedAt,
			ExpiresAt: expiresAt,
		},
		Busy:      false,
		Undecided: false,
	}
	return bc, taskQueues
}

// Issue #54 acceptance (a): a hang-release whose epoch had observed progress
// is a progress-interrupt — the task_dispatch Attempts budget is preserved,
// the release is counted on the ProgressInterrupts ledger, and (issue #55)
// the resume-eligible task is marked ResumeRequested for the next dispatch.
func TestHangRelease_ProgressInterrupt_PreservesAttemptsAndRequestsResume(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                "task_pi_a",
		CommandID:         "cmd_pi",
		LeaseEpoch:        3,
		Attempts:          3,
		LastProgressEpoch: 3, // progress observed at the released epoch
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.Status != model.StatusPending {
		t.Fatalf("Status = %s, want pending", got.Status)
	}
	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (progress-interrupt must not consume the dispatch budget)", got.Attempts)
	}
	if got.ProgressInterrupts != 1 {
		t.Errorf("ProgressInterrupts = %d, want 1", got.ProgressInterrupts)
	}
	if !got.ResumeRequested {
		t.Error("ResumeRequested = false, want true (resume-eligible task with resume budget remaining)")
	}
	if got.NotBefore == nil {
		t.Error("NotBefore not stamped; hang-release cooldown must still apply on the progress-interrupt path")
	}
}

// Issue #54: without observed progress at the released epoch the legacy
// accounting applies — the release consumes one Attempt.
func TestHangRelease_NoProgress_ConsumesAttempts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                "task_pi_b",
		CommandID:         "cmd_pi",
		LeaseEpoch:        3,
		Attempts:          3,
		LastProgressEpoch: 2, // progress last seen in a PRIOR epoch — this epoch was a wedge
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.Attempts != 4 {
		t.Errorf("Attempts = %d, want 4", got.Attempts)
	}
	if got.ProgressInterrupts != 0 {
		t.Errorf("ProgressInterrupts = %d, want 0", got.ProgressInterrupts)
	}
	if got.ResumeRequested {
		t.Error("ResumeRequested = true, want false")
	}
}

// Issue #54: the progress-interrupt exemption is bounded. At the
// retry.task_progress_interrupts cap the legacy accounting resumes so the
// task still reaches dead-letter eventually.
func TestHangRelease_ProgressInterruptCapExceeded_FallsBackToAttempts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{
		TaskDispatch:           5,
		TaskProgressInterrupts: ptr.Int(2),
	})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                 "task_pi_c",
		CommandID:          "cmd_pi",
		LeaseEpoch:         5,
		Attempts:           3,
		LastProgressEpoch:  5,
		ProgressInterrupts: 2, // ledger already at the cap
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.Attempts != 4 {
		t.Errorf("Attempts = %d, want 4 (cap exceeded → legacy accounting)", got.Attempts)
	}
	if got.ProgressInterrupts != 2 {
		t.Errorf("ProgressInterrupts = %d, want 2 (unchanged at cap)", got.ProgressInterrupts)
	}
	if got.ResumeRequested {
		t.Error("ResumeRequested = true, want false")
	}
}

// Issue #55 acceptance (c): a RunOnIntegration task mutates the shared
// integration tree, so it is progress-interrupt exempt (#54) but never
// resume-requested — recovery uses the /clear full re-dispatch.
func TestHangRelease_RunOnIntegration_NoResumeRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                "task_pi_d",
		CommandID:         "cmd_pi",
		LeaseEpoch:        3,
		Attempts:          1,
		LastProgressEpoch: 3,
		RunOnIntegration:  true,
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (progress-interrupt exemption still applies)", got.Attempts)
	}
	if got.ProgressInterrupts != 1 {
		t.Errorf("ProgressInterrupts = %d, want 1", got.ProgressInterrupts)
	}
	if got.ResumeRequested {
		t.Error("ResumeRequested = true, want false for RunOnIntegration")
	}
}

// Issue #55 acceptance (b): once the resume budget is exhausted the release
// still counts as a progress-interrupt (#54) but no resume is requested —
// the next dispatch is the /clear full envelope.
func TestHangRelease_ResumeBudgetExhausted_NoResumeRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{
		TaskDispatch: 5,
		TaskResume:   ptr.Int(2),
	})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                "task_pi_e",
		CommandID:         "cmd_pi",
		LeaseEpoch:        4,
		Attempts:          1,
		LastProgressEpoch: 4,
		ResumeAttempts:    2, // resume budget already consumed
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if got.ProgressInterrupts != 1 {
		t.Errorf("ProgressInterrupts = %d, want 1", got.ProgressInterrupts)
	}
	if got.ResumeRequested {
		t.Error("ResumeRequested = true, want false when resume budget is exhausted")
	}
}

// PR #56 review finding #1: with Attempts already at the task_dispatch max
// (charged at this epoch's acquisition), a progress-interrupt must refund the
// acquire-side charge — otherwise the dead-letter processor (which runs
// before dispatch in Phase A and consults neither NotBefore nor
// ResumeRequested) archives the task before the resume can happen.
func TestHangRelease_AttemptsAtMax_RefundPreventsDeadLetter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)
	cfg := model.RetryConfig{TaskDispatch: 5}
	qh := newProgressInterruptQH(t, now, cfg)

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                   "task_pi_boundary",
		CommandID:            "cmd_pi",
		LeaseEpoch:           5,
		Attempts:             5, // acquire walked it to the max this epoch
		AttemptsChargedEpoch: 5,
		LastProgressEpoch:    5,
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	tq := taskQueues["queue/worker1.yaml"]
	got := &tq.Queue.Tasks[0]
	if got.Attempts != 4 {
		t.Fatalf("Attempts = %d, want 4 (acquire charge refunded for the progress-interrupted epoch)", got.Attempts)
	}
	if got.AttemptsChargedEpoch != 0 {
		t.Errorf("AttemptsChargedEpoch = %d, want 0 (refund consumed)", got.AttemptsChargedEpoch)
	}
	if !got.ResumeRequested {
		t.Error("ResumeRequested = false, want true")
	}

	// The dead-letter processor must now retain the task: attempts dropped
	// below max, so the resume (and, on resume failure, the /clear full
	// fallback) can actually run.
	dlp := NewDeadLetterProcessor(testutil.SetupDirFixPerms(t), model.Config{Retry: cfg}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	dirty := false
	results := dlp.ProcessTaskDeadLetters(tq, &dirty)
	if len(results) != 0 {
		t.Fatalf("ProcessTaskDeadLetters removed the task: %+v", results)
	}
	if len(tq.Queue.Tasks) != 1 {
		t.Fatalf("task queue length = %d, want 1 (task retained for resume)", len(tq.Queue.Tasks))
	}
}

// An epoch acquired via the resume path charged ResumeAttempts, not Attempts
// (AttemptsChargedEpoch still points at an older epoch) — a progress-interrupt
// of that epoch must not refund anything.
func TestHangRelease_ResumeAcquiredEpoch_NoRefund(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:                   "task_pi_resume_epoch",
		CommandID:            "cmd_pi",
		LeaseEpoch:           4,
		Attempts:             2,
		AttemptsChargedEpoch: 3, // last Attempts charge was the PREVIOUS epoch
		LastProgressEpoch:    4,
		ResumeAttempts:       1,
	})
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (resume-acquired epoch must not be double-credited)", got.Attempts)
	}
	if got.ProgressInterrupts != 1 {
		t.Errorf("ProgressInterrupts = %d, want 1", got.ProgressInterrupts)
	}
}

// PR #56 review finding #2: retry clones must not inherit the per-instance
// progress/resume ledgers — an inherited LastProgressEpoch==1 would classify
// the clone's first epoch (LeaseEpoch restarts at 0 → 1) as "had progress"
// even when the clone wedges without output.
func TestCreateRetryTask_ResetsProgressAndResumeLedger(t *testing.T) {
	t.Parallel()
	original := &model.Task{
		ID:                   "task_orig",
		CommandID:            "cmd_orig",
		Purpose:              "test",
		Content:              "test content",
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
	handler := NewTaskRetryHandler(testutil.SetupDirFixPerms(t), model.Config{}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	clone, err := handler.CreateRetryTask(original, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if clone.LastProgressEpoch != 0 || clone.AttemptsChargedEpoch != 0 ||
		clone.ProgressInterrupts != 0 || clone.ResumeAttempts != 0 || clone.ResumeRequested {
		t.Errorf("ledger fields not reset: last_progress_epoch=%d attempts_charged_epoch=%d progress_interrupts=%d resume_attempts=%d resume_requested=%t",
			clone.LastProgressEpoch, clone.AttemptsChargedEpoch, clone.ProgressInterrupts, clone.ResumeAttempts, clone.ResumeRequested)
	}
	if clone.ResumeHint != model.ResumeHintDeny {
		t.Errorf("ResumeHint = %q, want %q (policy must be inherited)", clone.ResumeHint, model.ResumeHintDeny)
	}
}

// A confirmed-busy probe result stamps LastProgressEpoch for the current
// epoch (progress evidence equivalent to lease_extend_pane_active), fenced
// on the epoch so a stale Phase B result cannot stamp a newer lease.
func TestApplyTaskBusyCheck_BusyStampsProgressEpoch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:         "task_pi_f",
		CommandID:  "cmd_pi",
		LeaseEpoch: 2,
		Attempts:   1,
	})
	bc.Busy = true
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.LastProgressEpoch != 2 {
		t.Errorf("LastProgressEpoch = %d, want 2 (busy probe is progress evidence)", got.LastProgressEpoch)
	}
	if got.Status != model.StatusInProgress {
		t.Errorf("Status = %s, want in_progress (busy → lease extended)", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
}

// Issue #55 acceptance (a): a ResumeRequested task acquires its next lease as
// a resume dispatch — ResumeAttempts is consumed instead of Attempts, the
// marker is cleared, and the dispatch item carries Resume=true so Phase B
// delivers the continuation nudge without /clear.
func TestCollectPendingTaskDispatches_ResumeConsumesResumeBudget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	task := model.Task{
		ID:              "task_resume_collect",
		CommandID:       "cmd_resume",
		Status:          model.StatusPending,
		Attempts:        2,
		LeaseEpoch:      3,
		ResumeRequested: true,
		ResumeAttempts:  0,
		CreatedAt:       now.Format(time.RFC3339),
		UpdatedAt:       now.Format(time.RFC3339),
	}
	tq := &taskQueueEntry{Queue: model.TaskQueue{Tasks: []model.Task{task}}, Path: "queue/worker1.yaml"}
	work := &deferredWork{}

	dirty := qh.collectPendingTaskDispatches(tq, "worker1", map[string]bool{}, nil, map[string]*taskQueueEntry{"queue/worker1.yaml": tq}, nil, work)
	if !dirty {
		t.Fatal("expected dirty=true after lease acquisition")
	}
	if len(work.dispatches) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(work.dispatches))
	}
	item := work.dispatches[0]
	if !item.Resume {
		t.Error("dispatchItem.Resume = false, want true")
	}
	got := &tq.Queue.Tasks[0]
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (resume dispatch must not consume the dispatch budget)", got.Attempts)
	}
	if got.ResumeAttempts != 1 {
		t.Errorf("ResumeAttempts = %d, want 1", got.ResumeAttempts)
	}
	if got.ResumeRequested {
		t.Error("ResumeRequested = true, want false (marker consumed at acquisition)")
	}
	if got.LeaseEpoch != 4 {
		t.Errorf("LeaseEpoch = %d, want 4 (resume acquires a fresh epoch for result-write fencing)", got.LeaseEpoch)
	}
	if got.AttemptsChargedEpoch != 0 {
		t.Errorf("AttemptsChargedEpoch = %d, want 0 (resume acquisition must not stamp an Attempts charge)", got.AttemptsChargedEpoch)
	}
}

// Issue #55 acceptance (b): when the resume budget is already exhausted at
// acquisition time (defensive double-check of the hang-release gate), the
// dispatch falls back to a normal /clear full delivery and consumes Attempts.
func TestCollectPendingTaskDispatches_ResumeBudgetExhaustedFallsBack(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{
		TaskDispatch: 5,
		TaskResume:   ptr.Int(1),
	})

	task := model.Task{
		ID:              "task_resume_exhausted",
		CommandID:       "cmd_resume",
		Status:          model.StatusPending,
		Attempts:        2,
		ResumeRequested: true,
		ResumeAttempts:  1, // budget already spent
		CreatedAt:       now.Format(time.RFC3339),
		UpdatedAt:       now.Format(time.RFC3339),
	}
	tq := &taskQueueEntry{Queue: model.TaskQueue{Tasks: []model.Task{task}}, Path: "queue/worker1.yaml"}
	work := &deferredWork{}

	qh.collectPendingTaskDispatches(tq, "worker1", map[string]bool{}, nil, map[string]*taskQueueEntry{"queue/worker1.yaml": tq}, nil, work)
	if len(work.dispatches) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(work.dispatches))
	}
	if work.dispatches[0].Resume {
		t.Error("dispatchItem.Resume = true, want false (budget exhausted)")
	}
	got := &tq.Queue.Tasks[0]
	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (normal dispatch accounting)", got.Attempts)
	}
	if got.ResumeAttempts != 1 {
		t.Errorf("ResumeAttempts = %d, want 1 (unchanged)", got.ResumeAttempts)
	}
	if got.ResumeRequested {
		t.Error("ResumeRequested = true, want false (marker consumed)")
	}
	if got.AttemptsChargedEpoch != got.LeaseEpoch {
		t.Errorf("AttemptsChargedEpoch = %d, want %d (full acquisition stamps the charged epoch)", got.AttemptsChargedEpoch, got.LeaseEpoch)
	}
}

// Stale-epoch fencing: a busy result carrying an older epoch must not stamp
// progress onto the task's current lease.
func TestApplyTaskBusyCheck_StaleEpochDoesNotStampProgress(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	qh := newProgressInterruptQH(t, now, model.RetryConfig{TaskDispatch: 5})

	bc, taskQueues := hangReleaseFixture(now, model.Task{
		ID:         "task_pi_g",
		CommandID:  "cmd_pi",
		LeaseEpoch: 4,
		Attempts:   1,
	})
	bc.Busy = true
	bc.Item.Epoch = 3 // stale snapshot from a previous epoch
	qh.applyTaskBusyCheckResult(bc, taskQueues, map[string]bool{})

	got := &taskQueues["queue/worker1.yaml"].Queue.Tasks[0]
	if got.LastProgressEpoch != 0 {
		t.Errorf("LastProgressEpoch = %d, want 0 (stale epoch must be fenced)", got.LastProgressEpoch)
	}
}

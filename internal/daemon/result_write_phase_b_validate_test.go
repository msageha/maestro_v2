package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestValidateReportedResultTransition_AcceptsPreCompletionStates pins the
// audit-log relaxation introduced alongside the dispatch-time
// markTaskDispatched fix. A worker that lands at result_write with
// state=ready/dispatched/running is normal: those are the in-pipeline
// slots that a still-progressing task can occupy when AdvanceTaskState
// has not yet been driven through the full lifecycle hops. Phase B's
// applyTaskStateProgression handles the §2.1 routing via BFS, so flagging
// these source states as "invalid_state_transition" was operator noise.
func TestValidateReportedResultTransition_AcceptsPreCompletionStates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		existing model.Status
		recorded model.Status
		wantOK   bool
	}{
		{"ready -> completed: pre-progression worker report", model.StatusReady, model.StatusCompleted, true},
		{"dispatched -> completed: collect-phase advance only", model.StatusDispatched, model.StatusCompleted, true},
		{"running -> completed: classic happy path", model.StatusRunning, model.StatusCompleted, true},
		{"ready -> failed: pre-progression failure", model.StatusReady, model.StatusFailed, true},
		{"dispatched -> failed: dispatch-time failure", model.StatusDispatched, model.StatusFailed, true},
		{"running -> failed: classic failure", model.StatusRunning, model.StatusFailed, true},
		// Genuine violations must still be flagged so the audit log keeps
		// surfacing real state-machine drift. pending → completed is the
		// canonical "planner skipped ready/dispatch" red flag; the
		// repair_pending case represents a worker re-reporting a result
		// after the verify pipeline already routed the task into repair.
		{"pending -> completed: planner skipped ready/dispatch", model.StatusPending, model.StatusCompleted, false},
		{"repair_pending -> completed: belongs to repair pipeline", model.StatusRepairPending, model.StatusCompleted, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateReportedResultTransition(tc.existing, tc.recorded)
			if tc.wantOK && err != nil {
				t.Errorf("expected OK for %s -> %s, got error: %v", tc.existing, tc.recorded, err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("expected error for %s -> %s, got nil (genuine violation should still be flagged)", tc.existing, tc.recorded)
			}
		})
	}
}

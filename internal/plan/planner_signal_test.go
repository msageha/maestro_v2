package plan

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

func loadSignalQueueForTest(t *testing.T, maestroDir string) model.PlannerSignalQueue {
	t.Helper()
	data, err := os.ReadFile(plannerSignalQueuePath(maestroDir))
	if err != nil {
		t.Fatalf("read signal queue: %v", err)
	}
	var sq model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		t.Fatalf("unmarshal signal queue: %v", err)
	}
	return sq
}

func TestEmitPausedForReplanSignal_AppendsAndDeduplicates(t *testing.T) {
	t.Parallel()

	maestroDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(maestroDir, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	lockMap := lock.NewMutexMap()

	emitPausedForReplanSignal(maestroDir, "cmd_test_001", phaseSignalID("alpha"),
		"phase_fill_queue_write_rollback_failed", lockMap)
	emitPausedForReplanSignal(maestroDir, "cmd_test_001", phaseSignalID("alpha"),
		"phase_fill_queue_write_rollback_failed", lockMap) // duplicate

	sq := loadSignalQueueForTest(t, maestroDir)
	if got, want := len(sq.Signals), 1; got != want {
		t.Fatalf("len(signals) = %d, want %d (dedup should suppress second emit)", got, want)
	}
	got := sq.Signals[0]
	if got.Kind != "paused_for_replan" {
		t.Errorf("Kind = %q, want paused_for_replan", got.Kind)
	}
	if got.CommandID != "cmd_test_001" {
		t.Errorf("CommandID = %q", got.CommandID)
	}
	if got.PhaseID != "__phase_alpha" {
		t.Errorf("PhaseID = %q, want __phase_alpha", got.PhaseID)
	}
	if got.Reason != "phase_fill_queue_write_rollback_failed" {
		t.Errorf("Reason = %q", got.Reason)
	}
	if sq.SchemaVersion != 1 || sq.FileType != "planner_signal_queue" {
		t.Errorf("schema/file_type not initialized: %+v", sq)
	}

	// A different phase must produce a fresh entry (dedup is per-phase).
	emitPausedForReplanSignal(maestroDir, "cmd_test_001", phaseSignalID("beta"),
		"phase_fill_save_state_rollback_failed", lockMap)
	sq = loadSignalQueueForTest(t, maestroDir)
	if got, want := len(sq.Signals), 2; got != want {
		t.Fatalf("len(signals) = %d, want %d after distinct phase emit", got, want)
	}
}

func TestEmitPausedForReplanSignal_NilLockMap(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir()

	// Should be a no-op (logs error, returns silently); must not panic or
	// create the signal file.
	emitPausedForReplanSignal(maestroDir, "cmd_test_002", phaseSignalID("x"), "reason", nil)

	if _, err := os.Stat(plannerSignalQueuePath(maestroDir)); !os.IsNotExist(err) {
		t.Errorf("signal queue should not be created when lockMap is nil; stat err=%v", err)
	}
}

func TestPlannerSignalDuplicate(t *testing.T) {
	t.Parallel()
	signals := []model.PlannerSignal{
		{Kind: "paused_for_replan", CommandID: "c1", PhaseID: "__phase_a"},
		{Kind: "merge_conflict", CommandID: "c1", PhaseID: "__phase_b", WorkerID: "worker1"},
	}

	cases := []struct {
		name string
		sig  model.PlannerSignal
		want bool
	}{
		{"exact match", model.PlannerSignal{Kind: "paused_for_replan", CommandID: "c1", PhaseID: "__phase_a"}, true},
		{"different command", model.PlannerSignal{Kind: "paused_for_replan", CommandID: "c2", PhaseID: "__phase_a"}, false},
		{"different phase", model.PlannerSignal{Kind: "paused_for_replan", CommandID: "c1", PhaseID: "__phase_b"}, false},
		{"different kind", model.PlannerSignal{Kind: "merge_conflict", CommandID: "c1", PhaseID: "__phase_a"}, false},
		{"worker scoped match", model.PlannerSignal{Kind: "merge_conflict", CommandID: "c1", PhaseID: "__phase_b", WorkerID: "worker1"}, true},
		{"worker scoped mismatch", model.PlannerSignal{Kind: "merge_conflict", CommandID: "c1", PhaseID: "__phase_b", WorkerID: "worker2"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := plannerSignalDuplicate(signals, tc.sig); got != tc.want {
				t.Errorf("plannerSignalDuplicate(%+v) = %v, want %v", tc.sig, got, tc.want)
			}
		})
	}
}

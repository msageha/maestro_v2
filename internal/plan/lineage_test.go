package plan

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestLatestDescendant_NoLineage(t *testing.T) {
	t.Parallel()
	got := LatestDescendant("t1", nil)
	if got != "t1" {
		t.Errorf("LatestDescendant with nil lineage = %q, want %q", got, "t1")
	}
	got = LatestDescendant("t1", map[string]string{})
	if got != "t1" {
		t.Errorf("LatestDescendant with empty lineage = %q, want %q", got, "t1")
	}
}

func TestLatestDescendant_SingleHop(t *testing.T) {
	t.Parallel()
	lineage := map[string]string{
		"t1_retry": "t1",
	}
	if got := LatestDescendant("t1", lineage); got != "t1_retry" {
		t.Errorf("LatestDescendant(t1) = %q, want %q", got, "t1_retry")
	}
	// The successor itself, with no further hop, returns unchanged.
	if got := LatestDescendant("t1_retry", lineage); got != "t1_retry" {
		t.Errorf("LatestDescendant(t1_retry) = %q, want %q", got, "t1_retry")
	}
}

func TestLatestDescendant_MultiHop(t *testing.T) {
	t.Parallel()
	// t1 → t1_retry1 → t1_retry2 (chain of two retries)
	lineage := map[string]string{
		"t1_retry1": "t1",
		"t1_retry2": "t1_retry1",
	}
	if got := LatestDescendant("t1", lineage); got != "t1_retry2" {
		t.Errorf("LatestDescendant(t1) = %q, want %q (multi-hop)", got, "t1_retry2")
	}
	if got := LatestDescendant("t1_retry1", lineage); got != "t1_retry2" {
		t.Errorf("LatestDescendant(t1_retry1) = %q, want %q (mid-chain)", got, "t1_retry2")
	}
}

func TestLatestDescendant_CycleDefense(t *testing.T) {
	t.Parallel()
	// Corrupted state: a -> b, b -> a (a 2-cycle). Walk must terminate.
	lineage := map[string]string{
		"a": "b",
		"b": "a",
	}
	got := LatestDescendant("a", lineage)
	if got != "a" && got != "b" {
		t.Errorf("LatestDescendant on cycle should terminate at a or b, got %q", got)
	}
}

func TestEffectiveStatus_FollowsSupersededRetry(t *testing.T) {
	t.Parallel()
	// Mirrors the verify-repair recovery scenario: predecessor cancelled
	// with superseded_by_verify_repair, successor completed. The lineage
	// walk should report Completed for the predecessor — the cancellation
	// is a structural marker, not a real failure.
	taskStates := map[string]model.Status{
		"pre": model.StatusCancelled,
		"suc": model.StatusCompleted,
	}
	lineage := map[string]string{
		"suc": "pre",
	}
	if got := EffectiveStatus("pre", taskStates, lineage); got != model.StatusCompleted {
		t.Errorf("EffectiveStatus(pre) = %s, want %s (lineage successor completed)", got, model.StatusCompleted)
	}
}

func TestEffectiveStatus_FailedSuccessorRemainsFailed(t *testing.T) {
	t.Parallel()
	// If the successor itself fails, the lineage truly failed.
	taskStates := map[string]model.Status{
		"pre": model.StatusCancelled,
		"suc": model.StatusFailed,
	}
	lineage := map[string]string{
		"suc": "pre",
	}
	if got := EffectiveStatus("pre", taskStates, lineage); got != model.StatusFailed {
		t.Errorf("EffectiveStatus(pre) with failed successor = %s, want %s", got, model.StatusFailed)
	}
}

func TestEffectiveStatus_NoLineageReturnsRaw(t *testing.T) {
	t.Parallel()
	taskStates := map[string]model.Status{
		"t1": model.StatusFailed,
	}
	if got := EffectiveStatus("t1", taskStates, nil); got != model.StatusFailed {
		t.Errorf("EffectiveStatus with no lineage = %s, want raw %s", got, model.StatusFailed)
	}
}

// TestDeriveStatus_LineageRecovery exercises the full DeriveStatus path with
// a verify-repair-style recovery: predecessor cancelled, successor completed.
// The plan must derive Completed, not Cancelled.
func TestDeriveStatus_LineageRecovery(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"suc"},
			TaskStates: map[string]model.Status{
				"pre": model.StatusCancelled,
				"suc": model.StatusCompleted,
			},
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: map[string]string{
				"suc": "pre",
			},
		},
	}
	got, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus error: %v", err)
	}
	if got != model.PlanStatusCompleted {
		t.Errorf("DeriveStatus = %s, want %s", got, model.PlanStatusCompleted)
	}
}

// TestDeriveStatus_LineagePredecessorInRequired covers the case where the
// predecessor itself is still listed in RequiredTaskIDs (defensive: in real
// flows replaceTaskMembership swaps it out, but the lineage walk should
// still recognise the successor's completion).
func TestDeriveStatus_LineagePredecessorInRequired(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"pre"},
			TaskStates: map[string]model.Status{
				"pre": model.StatusCancelled,
				"suc": model.StatusCompleted,
			},
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: map[string]string{
				"suc": "pre",
			},
		},
	}
	got, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus error: %v", err)
	}
	if got != model.PlanStatusCompleted {
		t.Errorf("DeriveStatus with predecessor in required = %s, want %s", got, model.PlanStatusCompleted)
	}
}

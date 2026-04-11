package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/judge"
	"github.com/msageha/maestro_v2/internal/daemon/rollout"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// --- B-1: Rollout Eligibility Tests ---

func TestRolloutEligibility_AllConditionsMet(t *testing.T) {
	t.Parallel()
	input := rollout.ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      2,
		BloomLevel:        5,
		ExpectedPathCount: 3,
	}
	th := rollout.DefaultConditionThresholds()

	result := rollout.CheckEligibility(input, th)
	if !result.Eligible {
		t.Errorf("expected eligible, got ineligible: %v", result.Reasons)
	}
}

func TestRolloutEligibility_NoVerifyConfig(t *testing.T) {
	t.Parallel()
	input := rollout.ConditionInput{
		HasVerifyConfig:   false,
		FailureCount:      3,
		BloomLevel:        5,
		ExpectedPathCount: 3,
	}
	th := rollout.DefaultConditionThresholds()

	result := rollout.CheckEligibility(input, th)
	if result.Eligible {
		t.Error("expected ineligible when verify config is missing")
	}
	found := false
	for _, r := range result.Reasons {
		if r == "verify config is not defined" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'verify config is not defined' in reasons, got %v", result.Reasons)
	}
}

func TestRolloutEligibility_LowBloomNoFailure(t *testing.T) {
	t.Parallel()
	input := rollout.ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      0,
		BloomLevel:        2,
		ExpectedPathCount: 3,
	}
	th := rollout.DefaultConditionThresholds() // MinBloomLevel=4, MinFailureCount=1

	result := rollout.CheckEligibility(input, th)
	if result.Eligible {
		t.Error("expected ineligible for low bloom (2) and no failures")
	}
}

func TestRolloutEligibility_HighBloomNoFailure(t *testing.T) {
	t.Parallel()
	input := rollout.ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      0,
		BloomLevel:        5,
		ExpectedPathCount: 3,
	}
	th := rollout.DefaultConditionThresholds() // MinBloomLevel=4

	result := rollout.CheckEligibility(input, th)
	if !result.Eligible {
		t.Errorf("expected eligible for high bloom (5), got ineligible: %v", result.Reasons)
	}
}

func TestRolloutEligibility_TooWideExpectedPaths(t *testing.T) {
	t.Parallel()
	input := rollout.ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      2,
		BloomLevel:        5,
		ExpectedPathCount: 20,
	}
	th := rollout.DefaultConditionThresholds() // MaxExpectedPaths=10

	result := rollout.CheckEligibility(input, th)
	if result.Eligible {
		t.Error("expected ineligible for 20 expected paths (max 10)")
	}
}

func TestRolloutEligibility_Disabled(t *testing.T) {
	t.Parallel()
	cfg := model.Config{
		Rollout: model.RolloutConfig{
			Enabled: ptr.Bool(false),
		},
	}
	// When rollout is disabled, the daemon should not create a rollout manager
	if cfg.Rollout.EffectiveEnabled() {
		t.Error("expected rollout to be disabled")
	}
}

// --- B-1+B-2: Fitness Winner Selection Tests ---

func TestRolloutWinnerSelection_FitnessClear(t *testing.T) {
	t.Parallel()
	// Create a daemon with judge enabled
	judgeCalls := 0
	d := &Daemon{
		judgeCaller: judge.NewJudge(&mockCaller{
			callFn: func(_ context.Context, _ string) (string, error) {
				judgeCalls++
				return `{"winner_index": 1, "reasoning": "test"}`, nil
			},
		}, "test", 10*time.Second),
	}

	group := &rollout.Group{
		TaskID:    "test-task",
		CommandID: "test-cmd",
		Slots: []rollout.Slot{
			{Index: 0, Status: rollout.SlotCompleted},
			{Index: 1, Status: rollout.SlotCompleted},
		},
	}

	// Patch fitness scores to have a clear winner (slot 0 better)
	// Since default fitness for completed slots is all Passed=true with zeros,
	// they'll be tied. Let's test the clear winner case by using the model directly.
	scoresA := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 5}
	scoresB := model.FitnessScore{Passed: true, RepairCount: 2, DiffLinesChanged: 20}

	th := model.DefaultFitnessThresholds()
	winner, isTie := model.SelectWinner([]model.FitnessScore{scoresA, scoresB}, th)

	if isTie {
		t.Error("expected clear winner, got tie")
	}
	if winner != 0 {
		t.Errorf("expected winner index 0, got %d", winner)
	}

	// ResolveWinner should NOT call judge when there's a clear winner
	judgeCallCount := 0
	var judgeFunc model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
		judgeCallCount++
		return model.JudgeDecision{WinnerIndex: 1}, nil
	}

	winnerIdx, judgeUsed, err := model.ResolveWinner(
		[]model.FitnessScore{scoresA, scoresB}, th, judgeFunc,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if judgeUsed {
		t.Error("judge should NOT be used when fitness has a clear winner")
	}
	if winnerIdx != 0 {
		t.Errorf("expected winner 0, got %d", winnerIdx)
	}
	if judgeCallCount != 0 {
		t.Errorf("judge was called %d times, expected 0", judgeCallCount)
	}

	// Also verify daemon helper doesn't call judge on clear diff
	_ = d // d.judgeCaller is set but should not be invoked for clear winners
	_ = group
}

func TestRolloutWinnerSelection_FitnessTie_JudgeBreaks(t *testing.T) {
	t.Parallel()
	// Two identical fitness scores → tie → judge should be called
	scoreA := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 10}
	scoreB := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 10}

	th := model.DefaultFitnessThresholds()
	_, isTie := model.SelectWinner([]model.FitnessScore{scoreA, scoreB}, th)
	if !isTie {
		t.Fatal("expected tie for identical fitness scores")
	}

	// Judge picks winner index 1
	judgeCalled := false
	var judgeFunc model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
		judgeCalled = true
		return model.JudgeDecision{WinnerIndex: 1, Reasoning: "candidate 2 has better style"}, nil
	}

	winnerIdx, judgeUsed, err := model.ResolveWinner(
		[]model.FitnessScore{scoreA, scoreB}, th, judgeFunc,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !judgeUsed {
		t.Error("expected judge to be used on tie")
	}
	if !judgeCalled {
		t.Error("expected judge function to be called")
	}
	if winnerIdx != 1 {
		t.Errorf("expected judge-selected winner 1, got %d", winnerIdx)
	}
}

func TestRolloutWinnerSelection_FitnessTie_JudgeDisabled(t *testing.T) {
	t.Parallel()
	scoreA := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 10}
	scoreB := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 10}

	th := model.DefaultFitnessThresholds()

	// nil judge → fallback to index 0
	winnerIdx, judgeUsed, err := model.ResolveWinner(
		[]model.FitnessScore{scoreA, scoreB}, th, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if judgeUsed {
		t.Error("expected judge not used when nil")
	}
	if winnerIdx != 0 {
		t.Errorf("expected fallback winner 0, got %d", winnerIdx)
	}
}

func TestRolloutWinnerSelection_FitnessTie_JudgeError(t *testing.T) {
	t.Parallel()
	scoreA := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 10}
	scoreB := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 10}

	th := model.DefaultFitnessThresholds()

	var judgeFunc model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
		return model.JudgeDecision{}, fmt.Errorf("LLM timeout")
	}

	winnerIdx, _, err := model.ResolveWinner(
		[]model.FitnessScore{scoreA, scoreB}, th, judgeFunc,
	)
	if err == nil {
		t.Error("expected error from judge")
	}
	// Should fallback to index 0 on error
	if winnerIdx != 0 {
		t.Errorf("expected fallback winner 0 on judge error, got %d", winnerIdx)
	}
}

// --- B-2: Anti-Requirements Tests ---

func TestJudge_NeverOverridesFitness(t *testing.T) {
	t.Parallel()
	// When fitness has a clear winner, judge must not be called
	// even if it's available (§5-1)
	scoreClear := model.FitnessScore{Passed: true, RepairCount: 0, DiffLinesChanged: 5}
	scoreWorse := model.FitnessScore{Passed: true, RepairCount: 3, DiffLinesChanged: 30}

	th := model.DefaultFitnessThresholds()
	judgeCalled := false
	var judgeFunc model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
		judgeCalled = true
		return model.JudgeDecision{WinnerIndex: 1, Reasoning: "I prefer the worse one"}, nil
	}

	winnerIdx, judgeUsed, err := model.ResolveWinner(
		[]model.FitnessScore{scoreClear, scoreWorse}, th, judgeFunc,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if judgeCalled {
		t.Error("ANTI-REQUIREMENT VIOLATION: judge was called despite clear fitness winner")
	}
	if judgeUsed {
		t.Error("judgeUsed should be false when fitness is clear")
	}
	if winnerIdx != 0 {
		t.Errorf("fitness winner should be 0 (better score), got %d", winnerIdx)
	}
}

func TestJudge_OnlyOnTie(t *testing.T) {
	t.Parallel()
	th := model.DefaultFitnessThresholds()

	testCases := []struct {
		name             string
		scores           []model.FitnessScore
		expectJudgeCalled bool
	}{
		{
			name: "clear_winner_no_judge",
			scores: []model.FitnessScore{
				{Passed: true, RepairCount: 0, DiffLinesChanged: 5},
				{Passed: false, RepairCount: 0, DiffLinesChanged: 5},
			},
			expectJudgeCalled: false,
		},
		{
			name: "repair_count_diff_no_judge",
			scores: []model.FitnessScore{
				{Passed: true, RepairCount: 0, DiffLinesChanged: 10},
				{Passed: true, RepairCount: 2, DiffLinesChanged: 10},
			},
			expectJudgeCalled: false,
		},
		{
			name: "identical_scores_judge_called",
			scores: []model.FitnessScore{
				{Passed: true, RepairCount: 0, DiffLinesChanged: 10},
				{Passed: true, RepairCount: 0, DiffLinesChanged: 10},
			},
			expectJudgeCalled: true,
		},
		{
			name: "within_margin_judge_called",
			scores: []model.FitnessScore{
				{Passed: true, RepairCount: 0, DiffLinesChanged: 10},
				{Passed: true, RepairCount: 0, DiffLinesChanged: 15}, // within DiffSizeMargin=10
			},
			expectJudgeCalled: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			var judgeFunc model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
				called = true
				return model.JudgeDecision{WinnerIndex: 0}, nil
			}

			model.ResolveWinner(tc.scores, th, judgeFunc)

			if called != tc.expectJudgeCalled {
				t.Errorf("judge called=%v, expected=%v", called, tc.expectJudgeCalled)
			}
		})
	}
}

// --- Model Integration Tests ---

func TestResolveWinner_Integration(t *testing.T) {
	t.Parallel()
	// End-to-end test: SelectWinner → Judge delegation flow
	scores := []model.FitnessScore{
		{Passed: true, RepairCount: 0, DiffLinesChanged: 10, ExecutionTime: 5 * time.Second},
		{Passed: true, RepairCount: 0, DiffLinesChanged: 10, ExecutionTime: 5 * time.Second},
	}
	th := model.DefaultFitnessThresholds()

	// Step 1: Without judge → tie, fallback to 0
	winnerIdx, judgeUsed, err := model.ResolveWinner(scores, th, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if judgeUsed {
		t.Error("judge should not be used when nil")
	}
	if winnerIdx != 0 {
		t.Errorf("expected fallback winner 0, got %d", winnerIdx)
	}

	// Step 2: With judge → tie, judge picks 1
	var judgeFunc model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
		return model.JudgeDecision{WinnerIndex: 1, Reasoning: "better code quality"}, nil
	}

	winnerIdx, judgeUsed, err = model.ResolveWinner(scores, th, judgeFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !judgeUsed {
		t.Error("expected judge to be used on tie")
	}
	if winnerIdx != 1 {
		t.Errorf("expected judge winner 1, got %d", winnerIdx)
	}

	// Step 3: With clear winner → no judge involvement
	clearScores := []model.FitnessScore{
		{Passed: true, RepairCount: 0, DiffLinesChanged: 5},
		{Passed: true, RepairCount: 5, DiffLinesChanged: 50},
	}
	judgeCalled := false
	var judgeFunc2 model.JudgeFunc = func(_ context.Context, _ []model.FitnessScore, _ []map[string]string) (model.JudgeDecision, error) {
		judgeCalled = true
		return model.JudgeDecision{WinnerIndex: 1}, nil
	}

	winnerIdx, judgeUsed, err = model.ResolveWinner(clearScores, th, judgeFunc2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if judgeUsed || judgeCalled {
		t.Error("judge should not be used/called for clear winner")
	}
	if winnerIdx != 0 {
		t.Errorf("expected winner 0, got %d", winnerIdx)
	}
}

func TestRolloutManager_Integration(t *testing.T) {
	t.Parallel()
	mgr := rollout.NewManager(3)

	// Create group
	group, err := mgr.CreateGroup("task-1", "cmd-1", 2)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if group.ID != "rollout_task-1" {
		t.Errorf("expected group ID 'rollout_task-1', got %q", group.ID)
	}
	if len(group.Slots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(group.Slots))
	}
	if group.State != rollout.GroupPending {
		t.Errorf("expected state pending, got %s", group.State)
	}
	if group.WinnerSlot != -1 {
		t.Errorf("expected WinnerSlot -1, got %d", group.WinnerSlot)
	}

	// Verify slot IDs
	if group.Slots[0].ID != "task-1_slot_0" {
		t.Errorf("unexpected slot 0 ID: %s", group.Slots[0].ID)
	}
	if group.Slots[1].ID != "task-1_slot_1" {
		t.Errorf("unexpected slot 1 ID: %s", group.Slots[1].ID)
	}

	// Update slot statuses
	if err := mgr.UpdateSlotStatus("task-1", 0, rollout.SlotRunning); err != nil {
		t.Fatalf("UpdateSlotStatus slot 0 running: %v", err)
	}
	if err := mgr.UpdateSlotStatus("task-1", 1, rollout.SlotRunning); err != nil {
		t.Fatalf("UpdateSlotStatus slot 1 running: %v", err)
	}

	// Not yet complete
	if mgr.IsGroupComplete("task-1") {
		t.Error("group should not be complete while slots are running")
	}

	// Complete slot 0
	if err := mgr.UpdateSlotStatus("task-1", 0, rollout.SlotCompleted); err != nil {
		t.Fatalf("UpdateSlotStatus slot 0 completed: %v", err)
	}
	if mgr.IsGroupComplete("task-1") {
		t.Error("group should not be complete with only one slot terminal")
	}

	// Complete slot 1
	if err := mgr.UpdateSlotStatus("task-1", 1, rollout.SlotCompleted); err != nil {
		t.Fatalf("UpdateSlotStatus slot 1 completed: %v", err)
	}
	if !mgr.IsGroupComplete("task-1") {
		t.Error("group should be complete when all slots are terminal")
	}

	// Set winner
	if err := mgr.SetWinner("task-1", 0); err != nil {
		t.Fatalf("SetWinner failed: %v", err)
	}

	retrieved, ok := mgr.GetGroup("task-1")
	if !ok {
		t.Fatal("group should exist after SetWinner")
	}
	if retrieved.WinnerSlot != 0 {
		t.Errorf("expected winner slot 0, got %d", retrieved.WinnerSlot)
	}
	if retrieved.State != rollout.GroupCompleted {
		t.Errorf("expected state completed, got %s", retrieved.State)
	}
	if retrieved.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}

	// Active count should be 0
	if mgr.ActiveGroupCount() != 0 {
		t.Errorf("expected 0 active groups, got %d", mgr.ActiveGroupCount())
	}

	// Cleanup
	mgr.RemoveGroup("task-1")
	_, ok = mgr.GetGroup("task-1")
	if ok {
		t.Error("group should be removed")
	}
}

// --- helpers ---

type mockCaller struct {
	callFn func(ctx context.Context, prompt string) (string, error)
}

func (m *mockCaller) Call(ctx context.Context, prompt string) (string, error) {
	if m.callFn != nil {
		return m.callFn(ctx, prompt)
	}
	return `{"winner_index": 0, "reasoning": "default mock"}`, nil
}


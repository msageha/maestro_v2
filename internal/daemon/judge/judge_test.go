package judge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mockCaller is a simple test double for the Caller interface.
type mockCaller struct {
	response string
	err      error
}

func (m *mockCaller) Call(_ context.Context, _ string) (string, error) {
	return m.response, m.err
}

func twoCandidates() []CandidateInfo {
	return []CandidateInfo{
		{SlotIndex: 0, DiffSummary: "diff A", FitnessDesc: "score 85", FilesChanged: []string{"a.go"}, WorkerID: "w1"},
		{SlotIndex: 1, DiffSummary: "diff B", FitnessDesc: "score 85", FilesChanged: []string{"b.go"}, WorkerID: "w2"},
	}
}

func TestEvaluate_Winner1(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 1, "reasoning": "B is cleaner"}`,
	}, "test-model", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.WinnerIndex == nil || *d.WinnerIndex != 1 {
		t.Errorf("want WinnerIndex 1, got %v", d.WinnerIndex)
	}
	if d.Reasoning != "B is cleaner" {
		t.Errorf("unexpected reasoning: %s", d.Reasoning)
	}
	if d.Model != "test-model" {
		t.Errorf("want model test-model, got %s", d.Model)
	}
}

func TestEvaluate_Winner0(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": "A is better"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.WinnerIndex == nil || *d.WinnerIndex != 0 {
		t.Errorf("want WinnerIndex 0, got %v", d.WinnerIndex)
	}
}

func TestEvaluate_CallerError(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		err: errors.New("network failure"),
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "caller error") {
		t.Errorf("error should mention caller: %v", err)
	}
}

func TestEvaluate_InvalidJSON(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: "this is not json",
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected parse error")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse: %v", err)
	}
}

func TestEvaluate_Timeout(t *testing.T) {
	t.Parallel()
	slow := &slowCaller{delay: 2 * time.Second}
	j := NewJudge(slow, "m", 50*time.Millisecond)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "DeadlineExceeded") {
		// The error is wrapped, so check the message as well.
		if !strings.Contains(err.Error(), "caller error") {
			t.Errorf("expected deadline or caller error, got: %v", err)
		}
	}
}

type slowCaller struct{ delay time.Duration }

func (s *slowCaller) Call(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(s.delay):
		return `{"winner_index":0,"reasoning":"late"}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestEvaluate_NoCandidates(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{}, "m", 5*time.Second)

	_, err := j.Evaluate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty candidates")
	}
	if !strings.Contains(err.Error(), "no candidates") {
		t.Errorf("error should mention no candidates: %v", err)
	}
}

func TestEvaluate_SingleCandidate(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), []CandidateInfo{
		{SlotIndex: 3, DiffSummary: "only one", FitnessDesc: "90", FilesChanged: []string{"x.go"}, WorkerID: "w1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.WinnerIndex == nil || *d.WinnerIndex != 3 {
		t.Errorf("single candidate should win, want 3 got %v", d.WinnerIndex)
	}
}

func TestEvaluate_WinnerIndexNegative(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": -1, "reasoning": "bad"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for negative winner_index")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "not a valid candidate slot index") {
		t.Errorf("error should mention invalid slot index: %v", err)
	}
}

func TestEvaluate_WinnerIndexTooLarge(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 5, "reasoning": "bad"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for out-of-range winner_index")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "not a valid candidate slot index") {
		t.Errorf("error should mention invalid slot index: %v", err)
	}
}

func TestEvaluate_WinnerIndexNotInSlotIndices(t *testing.T) {
	t.Parallel()
	// Candidates have SlotIndex 10 and 20; LLM returns 0 which is not a valid slot index.
	candidates := []CandidateInfo{
		{SlotIndex: 10, DiffSummary: "diff A", FitnessDesc: "score 85", FilesChanged: []string{"a.go"}, WorkerID: "w1"},
		{SlotIndex: 20, DiffSummary: "diff B", FitnessDesc: "score 85", FilesChanged: []string{"b.go"}, WorkerID: "w2"},
	}
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": "wrong index"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), candidates)
	if err == nil {
		t.Fatal("expected error for winner_index not matching any SlotIndex")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
}

func TestEvaluate_UnknownFields(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": "A is better", "injected": "malicious"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for unknown fields")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure: %v", err)
	}
}

func TestEvaluate_EmptyReasoning(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": ""}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for empty reasoning")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "reasoning must not be empty") {
		t.Errorf("error should mention empty reasoning: %v", err)
	}
}

func TestEvaluate_WhitespaceOnlyReasoning(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": "   \n\t  "}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for whitespace-only reasoning")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "reasoning must not be empty") {
		t.Errorf("error should mention empty reasoning: %v", err)
	}
}

func TestEvaluate_ReasoningTooLong(t *testing.T) {
	t.Parallel()
	longReason := strings.Repeat("x", maxReasoningLength+1)
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": "` + longReason + `"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for too-long reasoning")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Errorf("error should mention length limit: %v", err)
	}
}

func TestEvaluate_WinnerIndexAsString(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": "zero", "reasoning": "A is better"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for string winner_index")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
}

func TestEvaluate_WinnerIndexAsFloat(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 1.5, "reasoning": "A is better"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for float winner_index")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "must be an integer") {
		t.Errorf("error should mention integer requirement: %v", err)
	}
}

func TestEvaluate_WinnerIndexAsNull(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": null, "reasoning": "A is better"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for null winner_index")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
}

func TestEvaluate_MissingWinnerIndex(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"reasoning": "A is better"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for missing winner_index")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
}

func TestEvaluate_MissingReasoning(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for missing reasoning")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "reasoning must not be empty") {
		t.Errorf("error should mention empty reasoning: %v", err)
	}
}

func TestEvaluate_TrailingContent(t *testing.T) {
	t.Parallel()
	j := NewJudge(&mockCaller{
		response: `{"winner_index": 0, "reasoning": "ok"} {"extra": "json"}`,
	}, "m", 5*time.Second)

	d, err := j.Evaluate(context.Background(), twoCandidates())
	if err == nil {
		t.Fatal("expected error for trailing content")
	}
	if d.WinnerIndex != nil {
		t.Errorf("error fallback should return nil WinnerIndex, got %v", d.WinnerIndex)
	}
	if !strings.Contains(err.Error(), "trailing content") {
		t.Errorf("error should mention trailing content: %v", err)
	}
}

func TestParseLLMResponse_ValidResponse(t *testing.T) {
	t.Parallel()
	resp, err := parseLLMResponse(`{"winner_index": 1, "reasoning": "B is cleaner"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.WinnerIndex.String() != "1" {
		t.Errorf("want winner_index 1, got %s", resp.WinnerIndex.String())
	}
	if resp.Reasoning != "B is cleaner" {
		t.Errorf("want reasoning 'B is cleaner', got %s", resp.Reasoning)
	}
}

func TestBuildPrompt_ContainsCandidateInfo(t *testing.T) {
	t.Parallel()
	candidates := twoCandidates()
	prompt := BuildPrompt(candidates)

	checks := []string{
		"slot 0", "slot 1",
		"diff A", "diff B",
		"score 85",
		"a.go", "b.go",
		"w1", "w2",
		"winner_index",
		"reasoning",
	}
	for _, want := range checks {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

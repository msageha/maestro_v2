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
	if d.WinnerIndex != 1 {
		t.Errorf("want WinnerIndex 1, got %d", d.WinnerIndex)
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
	if d.WinnerIndex != 0 {
		t.Errorf("want WinnerIndex 0, got %d", d.WinnerIndex)
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
	if d.WinnerIndex != -1 {
		t.Errorf("error fallback should return WinnerIndex -1, got %d", d.WinnerIndex)
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
	if d.WinnerIndex != -1 {
		t.Errorf("error fallback should return WinnerIndex -1, got %d", d.WinnerIndex)
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
	if d.WinnerIndex != -1 {
		t.Errorf("error fallback should return WinnerIndex -1, got %d", d.WinnerIndex)
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
	if d.WinnerIndex != 3 {
		t.Errorf("single candidate should win, want 3 got %d", d.WinnerIndex)
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

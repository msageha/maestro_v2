// Package judge evaluates worker outputs and selects the best result for rollout groups.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/ptr"
)

// CandidateInfo holds information about a single candidate for tie-breaking.
type CandidateInfo struct {
	SlotIndex    int
	DiffSummary  string
	FitnessDesc  string
	FilesChanged []string
	WorkerID     string
}

// Decision represents the judge's verdict.
// WinnerIndex is nil when no winner could be determined (error cases).
type Decision struct {
	WinnerIndex *int
	Reasoning   string
	Model       string
	Duration    time.Duration
}

// Caller abstracts the LLM call so it can be mocked in tests.
type Caller interface {
	Call(ctx context.Context, prompt string) (string, error)
}

// Judge evaluates competing candidates via an LLM call.
type Judge struct {
	caller  Caller
	model   string
	timeout time.Duration
}

// NewJudge creates a Judge with the given caller, model name, and per-call timeout.
func NewJudge(caller Caller, model string, timeout time.Duration) *Judge {
	return &Judge{
		caller:  caller,
		model:   model,
		timeout: timeout,
	}
}

// llmResponse is the expected JSON structure from the LLM.
type llmResponse struct {
	WinnerIndex int    `json:"winner_index"`
	Reasoning   string `json:"reasoning"`
}

// Evaluate asks the LLM to pick the best candidate.
// On any error the returned Decision has WinnerIndex nil and a non-nil error.
func (j *Judge) Evaluate(ctx context.Context, candidates []CandidateInfo) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{}, fmt.Errorf("judge: no candidates provided")
	}
	if len(candidates) == 1 {
		return Decision{
			WinnerIndex: ptr.Int(candidates[0].SlotIndex),
			Reasoning:   "single candidate — no tie-break needed",
			Model:       j.model,
		}, nil
	}

	prompt := BuildPrompt(candidates)

	ctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()

	start := time.Now()
	raw, err := j.caller.Call(ctx, prompt)
	elapsed := time.Since(start)

	if err != nil {
		return Decision{Model: j.model, Duration: elapsed}, fmt.Errorf("judge: caller error: %w", err)
	}

	var resp llmResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &resp); err != nil {
		return Decision{Model: j.model, Duration: elapsed}, fmt.Errorf("judge: failed to parse response: %w", err)
	}

	valid := false
	for _, c := range candidates {
		if resp.WinnerIndex == c.SlotIndex {
			valid = true
			break
		}
	}
	if !valid {
		return Decision{Model: j.model, Duration: elapsed},
			fmt.Errorf("judge: winner_index %d is not a valid candidate slot index", resp.WinnerIndex)
	}

	return Decision{
		WinnerIndex: ptr.Int(resp.WinnerIndex),
		Reasoning:   resp.Reasoning,
		Model:       j.model,
		Duration:    elapsed,
	}, nil
}

// BuildPrompt generates the structured prompt sent to the LLM.
func BuildPrompt(candidates []CandidateInfo) string {
	var b strings.Builder

	b.WriteString("You are an expert code reviewer. Compare the following code change candidates and choose the better one.\n\n")
	b.WriteString("## Evaluation Criteria\n")
	b.WriteString("1. Correctness of the code changes\n")
	b.WriteString("2. Maintainability and readability\n")
	b.WriteString("3. Minimality of changes (prefer smaller, focused diffs)\n")
	b.WriteString("4. Consistency with tests and acceptance criteria\n\n")

	for i, c := range candidates {
		fmt.Fprintf(&b, "## Candidate %d (slot %d, worker %s)\n", i+1, c.SlotIndex, c.WorkerID)
		fmt.Fprintf(&b, "- Fitness: %s\n", c.FitnessDesc)
		fmt.Fprintf(&b, "- Files changed: %s\n", strings.Join(c.FilesChanged, ", "))
		fmt.Fprintf(&b, "- Diff summary:\n%s\n\n", c.DiffSummary)
	}

	b.WriteString("## Output Format\n")
	b.WriteString("Respond with ONLY a JSON object (no markdown fences):\n")
	b.WriteString(`{"winner_index": <int>, "reasoning": "<string>"}`)
	b.WriteString("\n\nwinner_index must be one of the candidate slot indices.\n")

	return b.String()
}

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// defaultReviewerModel is used when a ReviewRequest omits ReviewerModel or
// provides an unsupported value.
const defaultReviewerModel = "claude-sonnet-4-6"

// ClaudeInvoker encapsulates the subprocess call to `claude` for one-shot
// review inference. It exists as an injection seam so tests can stub the
// transport without launching a real process.
type ClaudeInvoker interface {
	Invoke(ctx context.Context, model, systemPrompt, userPrompt string) (string, error)
}

// CLIInvoker is the default ClaudeInvoker that shells out to the `claude`
// binary in non-interactive (-p) mode with --output-format text.
type CLIInvoker struct{}

// Invoke launches `claude` as a short-lived subprocess and returns its
// stdout. The Claude CLI's -p flag runs a single turn and exits; output is
// plain text (we parse structured content from it ourselves).
func (CLIInvoker) Invoke(ctx context.Context, modelName, systemPrompt, userPrompt string) (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("resolve claude executable: %w", err)
	}
	if modelName == "" {
		modelName = defaultReviewerModel
	}
	if !isClaudeReviewModel(modelName) {
		return "", fmt.Errorf("%w: CLI reviewer only supports Claude models, got %q", ErrNotImplemented, modelName)
	}
	args := []string{
		"-p", userPrompt,
		"--model", modelName,
		"--append-system-prompt", systemPrompt,
		"--output-format", "text",
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is resolved via LookPath; args are constructed from validated config
	// Strip CLAUDECODE so nested invocation does not confuse the parent session.
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			filtered = append(filtered, e)
		}
	}
	cmd.Env = filtered

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("claude review invocation failed: %w; stderr=%s", err, truncate(stderr.String(), 512))
	}
	return stdout.String(), nil
}

func isClaudeReviewModel(modelName string) bool {
	switch modelName {
	case "", "sonnet", "opus", "haiku":
		return true
	}
	return strings.HasPrefix(modelName, "claude-")
}

const reviewSystemPrompt = `You are a senior code reviewer. Given a unified diff, return findings as a JSON array.

Output contract (REQUIRED):
- Output ONLY a single JSON array.
- Do not wrap in markdown code fences.
- Each element has fields: severity ("info"|"warning"|"error"), file_path (string), line (integer, 0 if not applicable), message (string), suggested_fix (string, optional).
- If there are no findings, output [].

Focus: correctness bugs, security vulnerabilities, resource leaks, race conditions, missing error handling, obvious performance regressions. Ignore pure style / formatting.`

// renderUserPrompt builds the per-request prompt. The diff is embedded in
// a plain literal block; we avoid asking the model for reasoning text so
// the output is amenable to JSON parsing.
func renderUserPrompt(req model.ReviewRequest) string {
	var b strings.Builder
	b.WriteString("Review the following diff and emit findings as a JSON array per the system contract.\n\n")
	if len(req.FilePaths) > 0 {
		b.WriteString("Files changed:\n")
		for _, p := range req.FilePaths {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}
	b.WriteString("Diff:\n")
	b.WriteString("---BEGIN DIFF---\n")
	b.WriteString(req.DiffContent)
	if !strings.HasSuffix(req.DiffContent, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("---END DIFF---\n")
	return b.String()
}

// parseFindings extracts a []model.ReviewFinding from the model's raw output.
// The function tolerates leading/trailing whitespace and a single outer
// markdown fence, but expects the core content to be valid JSON.
func parseFindings(raw string) ([]model.ReviewFinding, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("reviewer: empty model output")
	}
	// Strip a surrounding ```json ... ``` or ``` ... ``` fence if present.
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		if end := strings.LastIndex(s, "```"); end >= 0 {
			s = s[:end]
		}
		s = strings.TrimSpace(s)
	}
	// Seek to the first '[' to tolerate preamble text.
	if i := strings.Index(s, "["); i > 0 {
		s = s[i:]
	}
	// Seek back from the last ']' to tolerate trailing text.
	if j := strings.LastIndex(s, "]"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}

	var findings []model.ReviewFinding
	if err := json.Unmarshal([]byte(s), &findings); err != nil {
		return nil, fmt.Errorf("reviewer: parse findings JSON: %w", err)
	}
	// Normalize: default invalid severity to "info" so downstream aggregation
	// does not break on a malformed element.
	for i := range findings {
		if !model.ValidReviewSeverities[findings[i].Severity] {
			findings[i].Severity = model.ReviewSeverityInfo
		}
	}
	return findings, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

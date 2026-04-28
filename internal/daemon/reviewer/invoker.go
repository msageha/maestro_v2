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

// ClaudeInvoker encapsulates the subprocess call to a one-shot reviewer
// runtime (claude / codex / gemini). The interface name is historical; it
// is the injection seam tests use to stub the transport without launching
// a real subprocess.
type ClaudeInvoker interface {
	Invoke(ctx context.Context, model, systemPrompt, userPrompt string) (string, error)
}

// CLIInvoker is the default ClaudeInvoker. It dispatches the review call
// to whichever runtime the configured model name maps to (Claude, Codex,
// or Gemini), all via short-lived non-interactive subprocesses. The model
// name is interpreted via model.ParseRuntimeFromModel so the same naming
// convention used for worker runtimes (e.g. "codex", "gemini-2.5-pro")
// applies here.
type CLIInvoker struct{}

// Invoke runs the reviewer call against the runtime backing modelName and
// returns stdout. Errors carry the runtime's stderr (truncated) so log
// readers can diagnose CLI failures without re-running the subprocess.
func (CLIInvoker) Invoke(ctx context.Context, modelName, systemPrompt, userPrompt string) (string, error) {
	if modelName == "" {
		modelName = defaultReviewerModel
	}
	runtime, effectiveModel := model.ParseRuntimeFromModel(modelName)
	switch runtime {
	case model.RuntimeClaudeCode:
		return invokeClaude(ctx, effectiveModel, systemPrompt, userPrompt)
	case model.RuntimeCodex:
		return invokeCodex(ctx, effectiveModel, systemPrompt, userPrompt)
	case model.RuntimeGemini:
		return invokeGemini(ctx, effectiveModel, systemPrompt, userPrompt)
	default:
		// model.ParseRuntimeFromModel currently can only return one of the
		// three known runtimes; this branch is defensive against future
		// additions that forget to extend the switch.
		return "", fmt.Errorf("%w: reviewer runtime %q has no invoker", ErrNotImplemented, runtime)
	}
}

// invokeClaude shells out to `claude -p` (the CLI's non-interactive
// single-turn mode) with --append-system-prompt for the review system
// instruction.
func invokeClaude(ctx context.Context, modelName, systemPrompt, userPrompt string) (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("resolve claude executable: %w", err)
	}
	if modelName == "" {
		modelName = defaultReviewerModel
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

// invokeCodex shells out to `codex exec` (the CLI's non-interactive
// single-turn mode that prints the assistant response on stdout and
// exits). codex 0.125.0 has no flag for system instructions, so the
// system + user prompts are concatenated into one prompt body — codex
// receives them as a single user turn. The system block is wrapped in a
// "## Reviewer instructions" header so the model can still distinguish
// framing from the diff.
//
// `--skip-git-repo-check` is set because the reviewer subprocess is a
// short-lived headless invocation that doesn't need codex's project trust
// machinery; without the flag codex refuses to run when the temp cwd
// isn't a git repo. The same goes for `--sandbox read-only`: review only
// reads, never writes, so the most restrictive sandbox is correct.
func invokeCodex(ctx context.Context, modelName, systemPrompt, userPrompt string) (string, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("resolve codex executable: %w", err)
	}
	combined := combineReviewPrompt(systemPrompt, userPrompt)
	args := []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only"}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	args = append(args, combined)
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is resolved via LookPath; args are constructed from validated config
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("codex review invocation failed: %w; stderr=%s", err, truncate(stderr.String(), 512))
	}
	return stdout.String(), nil
}

// invokeGemini shells out to `gemini -p` (the CLI's headless mode that
// runs a single prompt and exits). Like codex, gemini-cli has no first-
// class system-instruction flag (the file-based GEMINI.md discovery is
// project/global, not invocation-scoped), so the system + user prompts
// are concatenated. `-p` always exits after one turn; that is the
// appropriate mode for an advisory reviewer.
func invokeGemini(ctx context.Context, modelName, systemPrompt, userPrompt string) (string, error) {
	bin, err := exec.LookPath("gemini")
	if err != nil {
		return "", fmt.Errorf("resolve gemini executable: %w", err)
	}
	combined := combineReviewPrompt(systemPrompt, userPrompt)
	args := []string{"-p", combined}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is resolved via LookPath; args are constructed from validated config
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("gemini review invocation failed: %w; stderr=%s", err, truncate(stderr.String(), 512))
	}
	return stdout.String(), nil
}

// combineReviewPrompt produces a single prompt body that opens with the
// reviewer's system instruction (under a clear header so the model can
// distinguish framing from data) followed by the user prompt. This is
// the workaround for runtimes that lack a dedicated system-prompt flag.
func combineReviewPrompt(systemPrompt, userPrompt string) string {
	var b strings.Builder
	b.Grow(len(systemPrompt) + len(userPrompt) + 64)
	if strings.TrimSpace(systemPrompt) != "" {
		b.WriteString("## Reviewer instructions\n")
		b.WriteString(systemPrompt)
		if !strings.HasSuffix(systemPrompt, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("\n## Review request\n")
	}
	b.WriteString(userPrompt)
	return b.String()
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

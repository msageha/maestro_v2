package reviewer

import (
	"strings"
	"testing"
)

func TestCombineReviewPrompt_HeaderedConcatenation(t *testing.T) {
	t.Parallel()
	got := combineReviewPrompt("system body", "user body")
	if !strings.Contains(got, "## Reviewer instructions\nsystem body") {
		t.Errorf("expected system block under reviewer instructions header; got %q", got)
	}
	if !strings.Contains(got, "## Review request\nuser body") {
		t.Errorf("expected user block under review request header; got %q", got)
	}
	// System block must precede the user block — codex / gemini receive
	// this as a single user turn so framing must come first.
	if strings.Index(got, "## Reviewer instructions") > strings.Index(got, "## Review request") {
		t.Errorf("system block must precede user block: %q", got)
	}
}

func TestCombineReviewPrompt_EmptySystemSkipsHeader(t *testing.T) {
	t.Parallel()
	got := combineReviewPrompt("   \n  ", "user body")
	if strings.Contains(got, "## Reviewer instructions") {
		t.Errorf("empty/whitespace-only system prompt should not produce a header block; got %q", got)
	}
	if got != "user body" {
		t.Errorf("expected raw user body when system is empty; got %q", got)
	}
}

func TestCombineReviewPrompt_AppendsTrailingNewlineToSystem(t *testing.T) {
	t.Parallel()
	// System prompt without trailing newline must still produce a clean
	// boundary before the next header so the model sees two distinct
	// markdown blocks.
	got := combineReviewPrompt("no trailing newline", "user body")
	if !strings.Contains(got, "no trailing newline\n\n## Review request") {
		t.Errorf("expected newline boundary between system and user blocks; got %q", got)
	}
}

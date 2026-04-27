// Package envelope provides functions for building delivery envelopes
// and sanitizing user-supplied content for the maestro agent protocol.
package envelope

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/msageha/maestro_v2/internal/model"
)

// Maximum byte lengths for envelope fields to prevent memory exhaustion (DoS).
// These limits are applied before sanitization.
const (
	MaxPurposeBytes            = 4096   // 4 KB
	MaxContentBytes            = 131072 // 128 KB
	MaxAcceptanceCriteriaBytes = 16384  // 16 KB
	MaxConstraintItemBytes     = 4096   // 4 KB per item
	MaxToolsHintItemBytes      = 512
	MaxPersonaHintBytes        = 512
	MaxSkillRefItemBytes       = 512
	MaxGenericFieldBytes       = 1024 // IDs, status strings in notification envelopes
)

// EnvelopeNoneLabel is the Japanese "none" label used in agent-facing
// envelopes. Keep this centralized so future locale changes touch one place.
const EnvelopeNoneLabel = "なし"

// TruncateUTF8Bytes truncates s to at most maxBytes, cutting at a valid UTF-8
// rune boundary. If truncation occurs, " [truncated]" is appended.
// Returns s unchanged if len(s) <= maxBytes or maxBytes <= 0.
func TruncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := 0
	for i, r := range s {
		next := i + utf8.RuneLen(r)
		if next > maxBytes {
			break
		}
		end = next
	}
	return s[:end] + " [truncated]"
}

// zeroWidthChars contains Unicode zero-width and invisible formatting characters
// that could be used to bypass text-based pattern matching.
var zeroWidthChars = strings.NewReplacer(
	"\u200B", "", // ZERO WIDTH SPACE
	"\u200C", "", // ZERO WIDTH NON-JOINER
	"\u200D", "", // ZERO WIDTH JOINER
	"\uFEFF", "", // ZERO WIDTH NO-BREAK SPACE (BOM)
	"\u2060", "", // WORD JOINER
	"\u00AD", "", // SOFT HYPHEN
	"\u180E", "", // MONGOLIAN VOWEL SEPARATOR
	"\u200E", "", // LEFT-TO-RIGHT MARK
	"\u200F", "", // RIGHT-TO-LEFT MARK
	"\u202A", "", // LEFT-TO-RIGHT EMBEDDING
	"\u202B", "", // RIGHT-TO-LEFT EMBEDDING
	"\u202C", "", // POP DIRECTIONAL FORMATTING
	"\u202D", "", // LEFT-TO-RIGHT OVERRIDE
	"\u202E", "", // RIGHT-TO-LEFT OVERRIDE
	"\u2066", "", // LEFT-TO-RIGHT ISOLATE
	"\u2067", "", // RIGHT-TO-LEFT ISOLATE
	"\u2068", "", // FIRST STRONG ISOLATE
	"\u2069", "", // POP DIRECTIONAL ISOLATE
)

// SanitizeEnvelopeField neutralises prompt-injection vectors in user-supplied
// envelope fields.  It performs the following transformations:
//  1. Applies NFKC Unicode normalization to canonicalize homoglyphs and
//     compatibility characters.
//  2. Removes zero-width and invisible formatting characters that could
//     bypass pattern matching.
//  3. Escapes "[maestro]" → "\\[maestro]" so injected content cannot mimic
//     system control headers.
//  4. Replaces newline (\n), U+2028 (Line Separator), and U+2029
//     (Paragraph Separator) with a space to prevent header injection.
//  5. Strips control characters (U+0000–U+001F) except tab (\t).
//
// Note: DATA boundary markers (BEGIN/END LEARNINGS/SKILLS/PERSONA) are
// sanitized separately via SanitizeUserContent before system sections are
// appended, to avoid escaping the system's own markers.
func SanitizeEnvelopeField(s string) string {
	s = norm.NFKC.String(s)
	s = zeroWidthChars.Replace(s)
	s = strings.ReplaceAll(s, "[maestro]", "\\[maestro]")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		if unicode.IsControl(r) && r != '\t' {
			return -1 // drop
		}
		return r
	}, s)
}

// SanitizeEnvelopeBody neutralises prompt-injection vectors in user-supplied
// multi-line body fields (content / acceptance_criteria / persona_hint) while
// preserving newlines so that markdown, bullet lists, and multi-line
// instructions reach the agent intact.
//
// Differences vs. SanitizeEnvelopeField:
//   - Line terminators (\r, \r\n, U+2028, U+2029) are normalized to \n instead
//     of being replaced with a space.
//   - \n and \t are preserved; all other control characters are dropped.
//
// The other defences (NFKC normalization, zero-width character removal,
// "[maestro]" escaping) are identical to SanitizeEnvelopeField.
//
// Use this only for fields whose envelope slot is textual content the agent
// needs to read as prose. Do NOT use on header-shaped fields (IDs, purpose,
// comma-joined lists) — those must stay on a single line to keep the
// "key: value" envelope format parseable.
func SanitizeEnvelopeBody(s string) string {
	s = norm.NFKC.String(s)
	s = zeroWidthChars.Replace(s)
	s = strings.ReplaceAll(s, "[maestro]", "\\[maestro]")
	// Normalize CRLF first so the \r pass below does not double-insert \n.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r', '\u2028', '\u2029':
			return '\n'
		}
		if unicode.IsControl(r) {
			return -1 // drop
		}
		return r
	}, s)
}

// boundaryMarkerPatterns matches DATA boundary markers case-insensitively
// to prevent bypass via case variations (e.g., "--- begin learnings").
var boundaryMarkerPatterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)---\s*BEGIN\s+LEARNINGS`), "--- BEGIN\\_LEARNINGS"},
	{regexp.MustCompile(`(?i)---\s*END\s+LEARNINGS`), "--- END\\_LEARNINGS"},
	{regexp.MustCompile(`(?i)---\s*BEGIN\s+SKILLS`), "--- BEGIN\\_SKILLS"},
	{regexp.MustCompile(`(?i)---\s*END\s+SKILLS`), "--- END\\_SKILLS"},
	{regexp.MustCompile(`(?i)---\s*BEGIN\s+PERSONA`), "--- BEGIN\\_PERSONA"},
	{regexp.MustCompile(`(?i)---\s*END\s+PERSONA`), "--- END\\_PERSONA"},
}

// SanitizeUserContent escapes DATA boundary markers in user-supplied content
// to prevent premature closing of LEARNINGS/SKILLS/PERSONA sections. This must
// be called on user content BEFORE system-generated sections are appended,
// so that the system's own markers remain intact.
//
// Matching is case-insensitive and tolerates variable whitespace between
// tokens to prevent bypass via "--- begin learnings" or "---  BEGIN  SKILLS".
func SanitizeUserContent(s string) string {
	for _, bm := range boundaryMarkerPatterns {
		s = bm.re.ReplaceAllString(s, bm.replacement)
	}
	return s
}

// sanitizeField truncates s to maxBytes at a UTF-8 boundary, then sanitizes it.
func sanitizeField(s string, maxBytes int) string {
	return SanitizeEnvelopeField(TruncateUTF8Bytes(s, maxBytes))
}

// sanitizeBodyField is the multi-line counterpart to sanitizeField: it
// truncates at a UTF-8 boundary and applies SanitizeEnvelopeBody so that
// newlines in the field are preserved.
func sanitizeBodyField(s string, maxBytes int) string {
	return SanitizeEnvelopeBody(TruncateUTF8Bytes(s, maxBytes))
}

// --- Typestate types for enforced sanitization pipeline ---
//
// The pipeline RawContent → SanitizedContent → TruncatedContent enforces at the
// type level that user content is sanitized (boundary markers escaped) before
// truncation and final field sanitization. Attempting to skip a step results in
// a compile error because the required method only exists on the preceding type.
//
// Example:
//
//	field := NewRawContent(userStr).Sanitize().Truncate(maxBytes).Build()
//
// Compile-time rejected (no Truncate on RawContent):
//
//	field := NewRawContent(userStr).Truncate(maxBytes) // does not compile

// RawContent wraps raw user-supplied content. The only valid operation is Sanitize.
type RawContent struct{ s string }

// NewRawContent wraps a raw user-supplied string for type-safe sanitization.
func NewRawContent(s string) RawContent { return RawContent{s: s} }

// Sanitize escapes DATA boundary markers (LEARNINGS/SKILLS/PERSONA) and returns
// SanitizedContent. This must be called before appending system sections or
// building envelope fields.
func (r RawContent) Sanitize() SanitizedContent {
	return SanitizedContent{s: SanitizeUserContent(r.s)}
}

// SanitizedContent represents content where DATA boundary markers have been
// escaped. System-generated sections can safely be appended or prepended.
type SanitizedContent struct{ s string }

// String returns the underlying sanitized string.
func (sc SanitizedContent) String() string { return sc.s }

// Append appends system-generated text (persona, skills, learnings) to the
// sanitized content and returns a new SanitizedContent.
func (sc SanitizedContent) Append(text string) SanitizedContent {
	return SanitizedContent{s: sc.s + text}
}

// Prepend prepends system-generated text to the sanitized content.
func (sc SanitizedContent) Prepend(text string) SanitizedContent {
	return SanitizedContent{s: text + sc.s}
}

// Truncate enforces byte-length limits at a valid UTF-8 boundary and returns
// TruncatedContent for the final build step.
func (sc SanitizedContent) Truncate(maxBytes int) TruncatedContent {
	return TruncatedContent{s: TruncateUTF8Bytes(sc.s, maxBytes)}
}

// TruncatedContent is sanitized and length-bounded content ready for final
// envelope field processing.
type TruncatedContent struct{ s string }

// Build applies final envelope field sanitization (NFKC normalization,
// zero-width char removal, [maestro] escaping, control char stripping) and
// returns the safe string for use in a single-line envelope field.
// Newlines are replaced with spaces — use BuildBody for multi-line slots.
func (tc TruncatedContent) Build() string {
	return SanitizeEnvelopeField(tc.s)
}

// BuildBody is the multi-line counterpart to Build: it applies the same
// prompt-injection defences but preserves newlines so the field remains
// usable as prose (markdown, bullet lists, multi-line instructions).
func (tc TruncatedContent) BuildBody() string {
	return SanitizeEnvelopeBody(tc.s)
}

// --- Envelope Builders ---

// BuildWorkerEnvelope creates the delivery envelope for a Worker task.
// The enrichedContent parameter must be a SanitizedContent obtained via
// NewRawContent(...).Sanitize(), ensuring boundary markers are escaped
// before envelope construction.
// Format matches spec §5.8.1 (Worker task envelope).
func BuildWorkerEnvelope(task model.Task, enrichedContent SanitizedContent, workerID string, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] task_id:%s command_id:%s lease_epoch:%d attempt:%d\n",
		task.ID, task.CommandID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "agent_id: %s\n", workerID)
	fmt.Fprintf(&sb, "purpose: %s\n", sanitizeField(task.Purpose, MaxPurposeBytes))
	// content / acceptance_criteria / persona_hint are multi-line prose fields:
	// preserve newlines so markdown, bullet lists, and multi-line instructions
	// reach the agent intact. See SanitizeEnvelopeBody for the defence model.
	fmt.Fprintf(&sb, "content: %s\n", enrichedContent.Truncate(MaxContentBytes).BuildBody())
	fmt.Fprintf(&sb, "acceptance_criteria: %s\n", sanitizeBodyField(task.AcceptanceCriteria, MaxAcceptanceCriteriaBytes))
	constraintsStr := EnvelopeNoneLabel
	if len(task.Constraints) > 0 {
		sanitized := make([]string, len(task.Constraints))
		for i, c := range task.Constraints {
			sanitized[i] = sanitizeField(c, MaxConstraintItemBytes)
		}
		constraintsStr = strings.Join(sanitized, ", ")
	}
	fmt.Fprintf(&sb, "constraints: %s\n", constraintsStr)
	toolsHintStr := EnvelopeNoneLabel
	if len(task.ToolsHint) > 0 {
		sanitizedHints := make([]string, len(task.ToolsHint))
		for i, h := range task.ToolsHint {
			sanitizedHints[i] = sanitizeField(h, MaxToolsHintItemBytes)
		}
		toolsHintStr = strings.Join(sanitizedHints, ", ")
	}
	fmt.Fprintf(&sb, "tools_hint: %s\n", toolsHintStr)
	personaHintStr := EnvelopeNoneLabel
	if task.PersonaHint != "" {
		personaHintStr = sanitizeBodyField(task.PersonaHint, MaxPersonaHintBytes)
	}
	fmt.Fprintf(&sb, "persona_hint: %s\n", personaHintStr)
	skillRefsStr := EnvelopeNoneLabel
	if len(task.SkillRefs) > 0 {
		sanitizedRefs := make([]string, len(task.SkillRefs))
		for i, r := range task.SkillRefs {
			sanitizedRefs[i] = sanitizeField(r, MaxSkillRefItemBytes)
		}
		skillRefsStr = strings.Join(sanitizedRefs, ", ")
	}
	fmt.Fprintf(&sb, "skill_refs: %s\n", skillRefsStr)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "完了時: maestro result write %s --task-id %s --command-id %s --lease-epoch %d --status <completed|failed> --summary \"...\"\n",
		workerID, task.ID, task.CommandID, leaseEpoch)
	sb.WriteString("失敗時に部分変更あり: 上記に加えて --partial-changes --no-retry-safe")
	return sb.String()
}

// BuildPlannerEnvelope creates the delivery envelope for a Planner command.
// The enrichedContent parameter must be a SanitizedContent obtained via
// NewRawContent(...).Sanitize(), ensuring boundary markers are escaped.
// Format matches spec §5.8.1 (Planner command envelope).
func BuildPlannerEnvelope(cmd model.Command, enrichedContent SanitizedContent, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] command_id:%s lease_epoch:%d attempt:%d\n",
		cmd.ID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "content: %s\n", enrichedContent.Truncate(MaxContentBytes).BuildBody())
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "タスク分解後: maestro plan submit --command-id %s --tasks-file -\n", cmd.ID)
	fmt.Fprintf(&sb, "全タスク完了後: maestro plan complete --command-id %s --summary \"...\"", cmd.ID)
	return sb.String()
}

// BuildOrchestratorNotificationEnvelope creates the envelope for an Orchestrator notification.
// Format matches spec §5.8.1 (Orchestrator notification envelope).
func BuildOrchestratorNotificationEnvelope(commandID string, notificationType model.NotificationType) string {
	terminalStatus := mapNotificationTypeToStatus(notificationType)
	return fmt.Sprintf("[maestro] kind:%s command_id:%s status:%s\nresults/planner.yaml を確認してください",
		sanitizeField(string(notificationType), MaxGenericFieldBytes),
		sanitizeField(commandID, MaxGenericFieldBytes),
		terminalStatus)
}

func mapNotificationTypeToStatus(nt model.NotificationType) string {
	switch nt {
	case "command_completed":
		return "completed"
	case "command_failed":
		return "failed"
	case "command_cancelled":
		return "cancelled"
	default:
		return string(nt)
	}
}

// BuildTaskResultNotification creates a side-channel notification for the Planner.
func BuildTaskResultNotification(commandID, taskID, workerID, taskStatus string) string {
	return fmt.Sprintf("[maestro] kind:task_result command_id:%s task_id:%s worker_id:%s status:%s\nresults/%s.yaml を確認してください",
		sanitizeField(commandID, MaxGenericFieldBytes),
		sanitizeField(taskID, MaxGenericFieldBytes),
		sanitizeField(workerID, MaxGenericFieldBytes),
		sanitizeField(taskStatus, MaxGenericFieldBytes),
		sanitizeField(workerID, MaxGenericFieldBytes))
}

// BuildTaskResultNotificationWithMaestroDir creates a Planner notification that
// includes absolute .maestro paths. Planner panes can run shell commands from
// arbitrary directories, so relative .maestro paths are ambiguous after a tool
// changes cwd.
func BuildTaskResultNotificationWithMaestroDir(commandID, taskID, workerID, taskStatus, maestroDir string) string {
	base := BuildTaskResultNotification(commandID, taskID, workerID, taskStatus)
	if maestroDir == "" {
		return base
	}
	resultPath := filepath.ToSlash(filepath.Join(maestroDir, "results", workerID+".yaml"))
	dashboardPath := filepath.ToSlash(filepath.Join(maestroDir, "dashboard.md"))
	return fmt.Sprintf("%s\n\n確認に使う .maestro は次の絶対パスです。他の .maestro は読まないでください。\n- result: %s\n- dashboard: %s",
		base,
		sanitizeField(resultPath, MaxGenericFieldBytes),
		sanitizeField(dashboardPath, MaxGenericFieldBytes))
}

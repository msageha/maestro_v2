// Package envelope provides functions for building delivery envelopes
// and sanitizing user-supplied content for the maestro agent protocol.
package envelope

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/msageha/maestro_v2/internal/model"
)

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
//  4. Replaces newline (\n) with a space to prevent header injection.
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
		if r == '\n' {
			return ' '
		}
		if unicode.IsControl(r) && r != '\t' {
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

// --- Envelope Builders ---

// BuildWorkerEnvelope creates the delivery envelope for a Worker task.
// Format matches spec §5.8.1 Worker 向けタスク配信エンベロープ.
func BuildWorkerEnvelope(task model.Task, workerID string, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] task_id:%s command_id:%s lease_epoch:%d attempt:%d\n",
		task.ID, task.CommandID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "agent_id: %s\n", workerID)
	fmt.Fprintf(&sb, "purpose: %s\n", SanitizeEnvelopeField(task.Purpose))
	fmt.Fprintf(&sb, "content: %s\n", SanitizeEnvelopeField(task.Content))
	fmt.Fprintf(&sb, "acceptance_criteria: %s\n", SanitizeEnvelopeField(task.AcceptanceCriteria))
	constraintsStr := "なし"
	if len(task.Constraints) > 0 {
		sanitized := make([]string, len(task.Constraints))
		for i, c := range task.Constraints {
			sanitized[i] = SanitizeEnvelopeField(c)
		}
		constraintsStr = strings.Join(sanitized, ", ")
	}
	fmt.Fprintf(&sb, "constraints: %s\n", constraintsStr)
	toolsHintStr := "なし"
	if len(task.ToolsHint) > 0 {
		sanitizedHints := make([]string, len(task.ToolsHint))
		for i, h := range task.ToolsHint {
			sanitizedHints[i] = SanitizeEnvelopeField(h)
		}
		toolsHintStr = strings.Join(sanitizedHints, ", ")
	}
	fmt.Fprintf(&sb, "tools_hint: %s\n", toolsHintStr)
	personaHintStr := "なし"
	if task.PersonaHint != "" {
		personaHintStr = SanitizeEnvelopeField(task.PersonaHint)
	}
	fmt.Fprintf(&sb, "persona_hint: %s\n", personaHintStr)
	skillRefsStr := "なし"
	if len(task.SkillRefs) > 0 {
		sanitizedRefs := make([]string, len(task.SkillRefs))
		for i, r := range task.SkillRefs {
			sanitizedRefs[i] = SanitizeEnvelopeField(r)
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
// Format matches spec §5.8.1 Planner 向けコマンド配信エンベロープ.
func BuildPlannerEnvelope(cmd model.Command, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] command_id:%s lease_epoch:%d attempt:%d\n",
		cmd.ID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "content: %s\n", SanitizeEnvelopeField(cmd.Content))
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "タスク分解後: maestro plan submit --command-id %s --tasks-file -\n", cmd.ID)
	fmt.Fprintf(&sb, "全タスク完了後: maestro plan complete --command-id %s --summary \"...\"", cmd.ID)
	return sb.String()
}

// BuildOrchestratorNotificationEnvelope creates the envelope for an Orchestrator notification.
// Format matches spec §5.8.1 Orchestrator 向け通知配信エンベロープ.
func BuildOrchestratorNotificationEnvelope(commandID string, notificationType model.NotificationType) string {
	terminalStatus := mapNotificationTypeToStatus(notificationType)
	return fmt.Sprintf("[maestro] kind:%s command_id:%s status:%s\nresults/planner.yaml を確認してください",
		SanitizeEnvelopeField(string(notificationType)),
		SanitizeEnvelopeField(commandID),
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
		SanitizeEnvelopeField(commandID),
		SanitizeEnvelopeField(taskID),
		SanitizeEnvelopeField(workerID),
		SanitizeEnvelopeField(taskStatus),
		SanitizeEnvelopeField(workerID))
}

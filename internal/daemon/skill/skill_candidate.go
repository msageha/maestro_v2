// Package skill provides utility functions for reading, writing, and merging
// skill candidate data accumulated from worker task results.
package skill

import (
	"fmt"
	"os"
	"slices"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// Size limits keeping state/skill_candidates.yaml structurally below the
// model.DefaultMaxYAMLFileBytes (5MiB) read guard. Worst case with all limits
// saturated: MaxCandidates x (MaxCandidateContentRunes runes, up to ~6 bytes
// each after YAML escaping, plus MaxCandidateCommandIDs command IDs) is about
// 2.5MiB — comfortably under MaxCandidatesFileBytes, which in turn sits under
// the read guard so a state file written by this package can always be read
// back.
const (
	// MaxCandidateContentRunes bounds a single candidate's content at
	// registration time; longer reports are truncated with a notice appended
	// (the learnings pipeline uses the same truncate-don't-reject policy).
	MaxCandidateContentRunes = 4096
	// MaxCandidateCommandIDs bounds the grounding command_ids list. Beyond
	// the cap, Occurrences keeps counting but no further IDs are recorded
	// (50 distinct observations already saturate every value gate).
	MaxCandidateCommandIDs = 50
	// MaxCandidates bounds the total number of retained candidates. Overflow
	// is pruned from terminal (approved/rejected) entries, oldest first;
	// pending entries are never pruned.
	MaxCandidates = 100
	// MaxCandidatesFileBytes is the serialized-size ceiling enforced by
	// WriteCandidates, kept below the read guard so writes made here can
	// never produce a file that ReadCandidates refuses.
	MaxCandidatesFileBytes = 4 * 1024 * 1024
)

// candidateTruncationNotice is appended to content cut at
// MaxCandidateContentRunes so operators reviewing the candidate know the
// report was not complete.
var candidateTruncationNotice = fmt.Sprintf("\n[maestro: content truncated to %d runes]", MaxCandidateContentRunes)

// ReadCandidates reads skill_candidates.yaml and returns the candidates list.
func ReadCandidates(path string) ([]model.SkillCandidate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a config file path from validated inputs
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skill candidates file: %w", err)
	}

	if len(data) > model.DefaultMaxYAMLFileBytes {
		return nil, fmt.Errorf(
			"skill candidates file %s exceeds size limit (%d > %d bytes); to recover, back the file up and move it aside, or hand-prune approved/rejected entries from it",
			path, len(data), model.DefaultMaxYAMLFileBytes)
	}
	var sf model.SkillCandidatesFile
	if err := yamlutil.SafeUnmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse skill candidates file: %w", err)
	}

	return sf.Candidates, nil
}

// WriteCandidates writes the candidates list to the given path as a
// SkillCandidatesFile. It refuses (leaving the on-disk state unchanged) when
// the serialized payload exceeds MaxCandidatesFileBytes: writing past that
// ceiling would poison the state file against the ReadCandidates size guard
// and freeze all candidate operations.
func WriteCandidates(path string, candidates []model.SkillCandidate) error {
	sf := model.SkillCandidatesFile{
		SchemaVersion: 1,
		FileType:      "state_skill_candidates",
		Candidates:    candidates,
	}

	data, err := yamlv3.Marshal(sf)
	if err != nil {
		return fmt.Errorf("marshal skill candidates file: %w", err)
	}
	if len(data) > MaxCandidatesFileBytes {
		return fmt.Errorf(
			"skill candidates state would serialize to %d bytes (limit %d); refusing to write, on-disk state unchanged",
			len(data), MaxCandidatesFileBytes)
	}
	if err := yamlutil.AtomicWriteRaw(path, data); err != nil {
		return fmt.Errorf("write skill candidates file: %w", err)
	}

	return nil
}

// EnforceCandidateCap prunes terminal (approved/rejected) candidates, oldest
// first (by UpdatedAt, falling back to CreatedAt), until at most maxEntries
// remain. Pending candidates are never pruned, so when pending entries alone
// exceed maxEntries the returned list is still over the cap and the caller
// decides how to proceed (registration skips the newest entry with a
// warning). Relative order of the surviving entries is preserved.
func EnforceCandidateCap(candidates []model.SkillCandidate, maxEntries int) ([]model.SkillCandidate, int) {
	pruned := 0
	for len(candidates) > maxEntries {
		oldest := -1
		for i, c := range candidates {
			if c.Status == "pending" {
				continue
			}
			if oldest == -1 || candidatePruneKey(c) < candidatePruneKey(candidates[oldest]) {
				oldest = i
			}
		}
		if oldest == -1 {
			break
		}
		candidates = slices.Delete(candidates, oldest, oldest+1)
		pruned++
	}
	return candidates, pruned
}

// candidatePruneKey returns the timestamp ordering prune decisions. RFC3339
// UTC strings compare chronologically as plain strings.
func candidatePruneKey(c model.SkillCandidate) string {
	if c.UpdatedAt != "" {
		return c.UpdatedAt
	}
	return c.CreatedAt
}

// AddOrUpdateCandidate merges a new skill candidate content into the existing list.
// A report matches an existing candidate when the whitespace-normalized content
// is identical, or when the token similarity reaches CandidateMergeThreshold
// (near-identical rewordings of the same pattern must accumulate into one
// Occurrences counter, because the 2x+ repetition threshold is the value gate
// for approval). On a match against a pending candidate, Occurrences is
// incremented and commandID appended (once per command); non-pending matches
// (approved/rejected candidates are frozen) are skipped. Otherwise a new
// pending candidate is created, annotated with similarSkills — the
// "<role>/<name>" references of existing library skills that already cover a
// similar pattern (dedup hint for the operator).
func AddOrUpdateCandidate(candidates []model.SkillCandidate, content, commandID, now string, idFunc func() (string, error), similarSkills []string) ([]model.SkillCandidate, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return candidates, nil
	}
	// Bound the stored content before matching so identical oversized reports
	// truncate identically and keep merging into one candidate.
	if runes := []rune(trimmed); len(runes) > MaxCandidateContentRunes {
		trimmed = string(runes[:MaxCandidateContentRunes]) + candidateTruncationNotice
	}

	if idx, matched := findMatchingCandidate(candidates, trimmed); matched {
		c := &candidates[idx]
		// Skip if not pending (approved/rejected candidates are frozen)
		if c.Status != "pending" {
			return candidates, nil
		}
		// Only increment if this commandID hasn't been recorded yet. Past the
		// MaxCandidateCommandIDs cap no further IDs are recorded (Occurrences
		// keeps counting; the grounding list is already saturated).
		if !slices.Contains(c.CommandIDs, commandID) {
			c.Occurrences++
			if len(c.CommandIDs) < MaxCandidateCommandIDs {
				c.CommandIDs = append(c.CommandIDs, commandID)
			}
			c.UpdatedAt = now
		}
		return candidates, nil
	}

	// New candidate
	id, err := idFunc()
	if err != nil {
		return candidates, fmt.Errorf("generate skill candidate ID: %w", err)
	}

	candidates = append(candidates, model.SkillCandidate{
		ID:            id,
		Content:       trimmed,
		Occurrences:   1,
		CommandIDs:    []string{commandID},
		CreatedAt:     now,
		UpdatedAt:     now,
		Status:        "pending",
		SimilarSkills: slices.Clone(similarSkills),
	})

	return candidates, nil
}

// findMatchingCandidate locates the existing candidate the new content should
// merge into: first by exact whitespace-normalized equality, then by token
// similarity at CandidateMergeThreshold. Exact matches win over fuzzy ones so
// a verbatim re-report always lands on its original entry.
func findMatchingCandidate(candidates []model.SkillCandidate, content string) (int, bool) {
	normalized := normalizeCandidateContent(content)
	for i, c := range candidates {
		if normalizeCandidateContent(c.Content) == normalized {
			return i, true
		}
	}
	for i, c := range candidates {
		if TokenJaccard(c.Content, content) >= CandidateMergeThreshold {
			return i, true
		}
	}
	return -1, false
}

// normalizeCandidateContent collapses all whitespace runs to a single space
// so formatting-only differences (line wrapping, indentation) do not split
// the occurrence count across duplicate candidates.
func normalizeCandidateContent(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

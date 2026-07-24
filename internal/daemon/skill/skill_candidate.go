// Package skill provides utility functions for reading, writing, and merging
// skill candidate data accumulated from worker task results.
package skill

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

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
		return nil, fmt.Errorf("skill candidates file exceeds size limit (%d > %d bytes)", len(data), model.DefaultMaxYAMLFileBytes)
	}
	var sf model.SkillCandidatesFile
	if err := yamlutil.SafeUnmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse skill candidates file: %w", err)
	}

	return sf.Candidates, nil
}

// WriteCandidates writes the candidates list to the given path as a SkillCandidatesFile.
func WriteCandidates(path string, candidates []model.SkillCandidate) error {
	sf := model.SkillCandidatesFile{
		SchemaVersion: 1,
		FileType:      "state_skill_candidates",
		Candidates:    candidates,
	}

	if err := yamlutil.AtomicWrite(path, sf); err != nil {
		return fmt.Errorf("write skill candidates file: %w", err)
	}

	return nil
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

	if idx, matched := findMatchingCandidate(candidates, trimmed); matched {
		c := &candidates[idx]
		// Skip if not pending (approved/rejected candidates are frozen)
		if c.Status != "pending" {
			return candidates, nil
		}
		// Only increment if this commandID hasn't been recorded yet
		if !slices.Contains(c.CommandIDs, commandID) {
			c.Occurrences++
			c.CommandIDs = append(c.CommandIDs, commandID)
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

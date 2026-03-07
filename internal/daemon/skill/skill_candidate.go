// Package skill provides utility functions for reading, writing, and merging
// skill candidate data accumulated from worker task results.
package skill

import (
	"fmt"
	"os"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// ReadCandidates reads skill_candidates.yaml and returns the candidates list.
func ReadCandidates(path string) ([]model.SkillCandidate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skill candidates file: %w", err)
	}

	var sf model.SkillCandidatesFile
	if err := yamlv3.Unmarshal(data, &sf); err != nil {
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
// If the same content already exists, Occurrences is incremented and commandID is
// appended (if not already present). If the candidate is not pending, it is skipped.
// Otherwise a new candidate is created with status "pending".
func AddOrUpdateCandidate(candidates []model.SkillCandidate, content, commandID, now string, idFunc func() (string, error)) ([]model.SkillCandidate, error) {
	normalized := strings.TrimSpace(content)
	if normalized == "" {
		return candidates, nil
	}

	for i, c := range candidates {
		if c.Content == normalized {
			// Skip if not pending (approved/rejected candidates are frozen)
			if c.Status != "pending" {
				return candidates, nil
			}
			// Only increment if this commandID hasn't been recorded yet
			if !containsString(c.CommandIDs, commandID) {
				candidates[i].Occurrences++
				candidates[i].CommandIDs = append(candidates[i].CommandIDs, commandID)
				candidates[i].UpdatedAt = now
			}
			return candidates, nil
		}
	}

	// New candidate
	id, err := idFunc()
	if err != nil {
		return candidates, fmt.Errorf("generate skill candidate ID: %w", err)
	}

	candidates = append(candidates, model.SkillCandidate{
		ID:          id,
		Content:     normalized,
		Occurrences: 1,
		CommandIDs:  []string{commandID},
		CreatedAt:   now,
		UpdatedAt:   now,
		Status:      "pending",
	})

	return candidates, nil
}

func containsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

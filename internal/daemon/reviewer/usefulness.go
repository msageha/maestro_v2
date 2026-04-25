// Package reviewer provides reviewer usefulness tracking and statistics.
package reviewer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ReviewResult is the usefulness-tracking projection of a completed review.
// It is intentionally narrower than model.ReviewResult: usefulness only needs
// the reviewer/task/command identifiers and the IDs of findings produced, not
// full finding bodies or status — those live in the canonical model type.
type ReviewResult struct {
	ReviewerModel string
	TaskID        string
	CommandID     string
	FindingIDs    []string
}

// UsefulnessRecord represents a single review result record with adoption data.
type UsefulnessRecord struct {
	ReviewerModel string    `json:"reviewer_model"`
	TaskID        string    `json:"task_id"`
	CommandID     string    `json:"command_id"`
	FindingCount  int       `json:"finding_count"`
	AdoptedCount  int       `json:"adopted_count"`
	Timestamp     time.Time `json:"timestamp"`
}

// ModelStats contains aggregated usefulness statistics for a single model.
type ModelStats struct {
	Model           string  `json:"model"`
	TotalFindings   int     `json:"total_findings"`
	AdoptedFindings int     `json:"adopted_findings"`
	AdoptionRate    float64 `json:"adoption_rate"`
	ReviewCount     int     `json:"review_count"`
}

const maxUsefulnessRecords = 10000

// UsefulnessTracker records and aggregates reviewer usefulness data.
type UsefulnessTracker struct {
	mu       sync.Mutex
	fileMu   sync.Mutex
	records  []UsefulnessRecord
	filePath string
}

// NewUsefulnessTracker creates a tracker that persists to dir/reviewer_usefulness.jsonl.
// If the file already exists, existing records are loaded into memory.
func NewUsefulnessTracker(dir string) (*UsefulnessTracker, error) {
	filePath := filepath.Join(dir, "reviewer_usefulness.jsonl")
	t := &UsefulnessTracker{
		filePath: filePath,
	}
	if err := t.load(); err != nil {
		return nil, fmt.Errorf("load usefulness data: %w", err)
	}
	return t, nil
}

// RecordResult records a review result along with which findings were adopted.
func (t *UsefulnessTracker) RecordResult(result ReviewResult, adoptedFindingIDs []string) error {
	adoptedSet := make(map[string]struct{}, len(adoptedFindingIDs))
	for _, id := range adoptedFindingIDs {
		adoptedSet[id] = struct{}{}
	}

	adoptedCount := 0
	for _, id := range result.FindingIDs {
		if _, ok := adoptedSet[id]; ok {
			adoptedCount++
		}
	}

	rec := UsefulnessRecord{
		ReviewerModel: result.ReviewerModel,
		TaskID:        result.TaskID,
		CommandID:     result.CommandID,
		FindingCount:  len(result.FindingIDs),
		AdoptedCount:  adoptedCount,
		Timestamp:     time.Now(),
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.records = append(t.records, rec)
	if len(t.records) > maxUsefulnessRecords {
		t.records = t.records[len(t.records)-maxUsefulnessRecords:]
	}

	if err := t.appendToFile(rec); err != nil {
		return fmt.Errorf("persist usefulness record: %w", err)
	}
	return nil
}

// GetModelStats returns aggregated statistics for the specified model.
func (t *UsefulnessTracker) GetModelStats(model string) ModelStats {
	t.mu.Lock()
	defer t.mu.Unlock()

	stats := ModelStats{Model: model}
	for _, r := range t.records {
		if r.ReviewerModel == model {
			stats.TotalFindings += r.FindingCount
			stats.AdoptedFindings += r.AdoptedCount
			stats.ReviewCount++
		}
	}
	if stats.TotalFindings > 0 {
		stats.AdoptionRate = float64(stats.AdoptedFindings) / float64(stats.TotalFindings)
	}
	return stats
}

// GetAllModelStats returns aggregated statistics for all models that have records.
func (t *UsefulnessTracker) GetAllModelStats() []ModelStats {
	t.mu.Lock()
	defer t.mu.Unlock()

	byModel := make(map[string]*ModelStats)
	for _, r := range t.records {
		s, ok := byModel[r.ReviewerModel]
		if !ok {
			s = &ModelStats{Model: r.ReviewerModel}
			byModel[r.ReviewerModel] = s
		}
		s.TotalFindings += r.FindingCount
		s.AdoptedFindings += r.AdoptedCount
		s.ReviewCount++
	}

	out := make([]ModelStats, 0, len(byModel))
	for _, s := range byModel {
		if s.TotalFindings > 0 {
			s.AdoptionRate = float64(s.AdoptedFindings) / float64(s.TotalFindings)
		}
		out = append(out, *s)
	}
	return out
}

func (t *UsefulnessTracker) load() error {
	f, err := os.Open(t.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("close usefulness file failed", "error", err)
		}
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec UsefulnessRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			slog.Warn("usefulness load: skipping malformed JSON line", "error", err)
			continue
		}
		t.records = append(t.records, rec)
	}
	return scanner.Err()
}

func (t *UsefulnessTracker) appendToFile(rec UsefulnessRecord) error {
	t.fileMu.Lock()
	defer t.fileMu.Unlock()

	f, err := os.OpenFile(t.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("close usefulness file failed", "error", err)
		}
	}()

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

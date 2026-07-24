package hud

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// History retention policy. The file is HUD-owned (the daemon never touches
// it): records older than historyRetention are pruned at load time, and the
// on-disk file is compacted when pruning removed anything or the file grew
// past historyMaxBytes.
const (
	historyRetention = 7 * 24 * time.Hour
	historyMaxBytes  = 4 * 1024 * 1024
	// historyMaxRecords caps in-memory records as a final backstop against
	// a pathological file.
	historyMaxRecords = 100_000
)

// Gauges is the numeric vector persisted per history record. It must stay
// comparable (==) — append dedup relies on it.
type Gauges struct {
	CommandsDispatched int     `json:"commands_dispatched"`
	TasksDispatched    int     `json:"tasks_dispatched"`
	TasksCompleted     int     `json:"tasks_completed"`
	TasksFailed        int     `json:"tasks_failed"`
	TasksCancelled     int     `json:"tasks_cancelled"`
	DeadLetters        int     `json:"dead_letters"`
	QueuePending       int     `json:"queue_pending"`
	QueueInProgress    int     `json:"queue_in_progress"`
	LearningsTotal     int     `json:"learnings_total"`
	SkillCandidates    int     `json:"skill_candidates"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	EstimatedCostUSD   float64 `json:"estimated_cost_usd"`
}

// Record is one JSONL history line: a timestamp plus the gauge vector.
type Record struct {
	TS string `json:"ts"` // RFC3339
	Gauges
}

// SnapshotGauges projects a Snapshot onto the persisted gauge vector.
func SnapshotGauges(s *Snapshot) Gauges {
	g := Gauges{
		CommandsDispatched: s.Metrics.Counters.CommandsDispatched,
		TasksDispatched:    s.Metrics.Counters.TasksDispatched,
		TasksCompleted:     s.Metrics.Counters.TasksCompleted,
		TasksFailed:        s.Metrics.Counters.TasksFailed,
		TasksCancelled:     s.Metrics.Counters.TasksCancelled,
		DeadLetters:        s.Metrics.Counters.DeadLetters,
		LearningsTotal:     s.Learnings.Total,
		SkillCandidates:    s.SkillCandidates.Pending + s.SkillCandidates.Approved + s.SkillCandidates.Rejected,
	}
	for _, q := range s.Queues.Rows {
		g.QueuePending += q.Pending
		g.QueueInProgress += q.InProgress
	}
	if u := s.Metrics.Usage; u != nil {
		for _, a := range u.Agents {
			if a == nil || !a.TokensKnown {
				continue
			}
			g.InputTokens += a.Totals.InputTokens
			g.OutputTokens += a.Totals.OutputTokens
			if a.EstimatedCostUSD != nil {
				g.EstimatedCostUSD += *a.EstimatedCostUSD
			}
		}
	}
	return g
}

// History is the loaded (pruned) snapshot-diff trail.
type History struct {
	path    string
	records []Record
	// needsCompact is set when loading pruned records or found an oversized
	// file. Compaction is deferred to the first Append so a pure read
	// (--once mode) never writes anything.
	needsCompact bool
}

// LoadHistory reads and prunes the JSONL history at path. A missing file is
// an empty history. Unparseable lines are skipped (the HUD must not die on
// its own trail). Loading itself never writes: when pruning removed records
// or the file exceeded historyMaxBytes, the file is compacted atomically
// (write temp + rename) on the next Append. path=="" returns an
// in-memory-only history that never persists.
func LoadHistory(path string, now time.Time) (*History, error) {
	h := &History{path: path}
	if path == "" {
		return h, nil
	}
	f, err := os.Open(path) //nolint:gosec // HUD-owned state path
	if err != nil {
		if os.IsNotExist(err) {
			return h, nil
		}
		return h, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return h, err
	}

	cutoff := now.Add(-historyRetention)
	var kept []Record
	dropped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			dropped++
			continue
		}
		ts, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil || ts.Before(cutoff) {
			dropped++
			continue
		}
		kept = append(kept, rec)
		if len(kept) > historyMaxRecords {
			kept = kept[1:]
			dropped++
		}
	}
	if err := sc.Err(); err != nil {
		return h, err
	}
	h.records = kept
	h.needsCompact = dropped > 0 || info.Size() > historyMaxBytes
	return h, nil
}

// compact rewrites the on-disk file from the in-memory records via a temp
// file + rename so a crash never leaves a truncated history.
func (h *History) compact() error {
	if h.path == "" {
		return nil
	}
	dir := filepath.Dir(h.path)
	tmp, err := os.CreateTemp(dir, ".hud_history-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	w := bufio.NewWriter(tmp)
	for i := range h.records {
		line, err := json.Marshal(&h.records[i])
		if err != nil {
			_ = tmp.Close()
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, h.path)
}

// Append records rec unless its gauge vector equals the latest record's
// (dedup keeps the trail change-driven instead of one line per poll).
// Returns whether the record was appended.
func (h *History) Append(rec Record) (bool, error) {
	if last := h.Latest(); last != nil && last.Gauges == rec.Gauges {
		return false, nil
	}
	h.records = append(h.records, rec)
	if h.path == "" {
		return true, nil
	}
	if h.needsCompact {
		// Deferred load-time pruning: rewrite once, which also lands rec.
		if err := h.compact(); err != nil {
			return true, err
		}
		h.needsCompact = false
		return true, nil
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // HUD-owned state path
	if err != nil {
		return false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("append history: %w", err)
	}
	return true, f.Close()
}

// Latest returns the most recent record, or nil for an empty history.
func (h *History) Latest() *Record {
	if len(h.records) == 0 {
		return nil
	}
	return &h.records[len(h.records)-1]
}

// Previous returns the record just before the latest one, or nil.
func (h *History) Previous() *Record {
	if len(h.records) < 2 {
		return nil
	}
	return &h.records[len(h.records)-2]
}

// Baseline returns the newest record at least age older than now — the
// "since yesterday" anchor when age is 24h. When no record is old enough it
// falls back to the oldest record so a young trail still shows growth since
// its own beginning. Returns nil for an empty history.
func (h *History) Baseline(now time.Time, age time.Duration) *Record {
	if len(h.records) == 0 {
		return nil
	}
	cutoff := now.Add(-age)
	var newest *Record
	for i := range h.records {
		ts, err := time.Parse(time.RFC3339, h.records[i].TS)
		if err != nil {
			continue
		}
		if !ts.After(cutoff) {
			newest = &h.records[i]
		} else {
			break // records are appended chronologically
		}
	}
	if newest == nil {
		return &h.records[0]
	}
	return newest
}

// Len returns the number of loaded records.
func (h *History) Len() int { return len(h.records) }

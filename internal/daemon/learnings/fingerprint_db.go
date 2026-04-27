package learnings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FailurePattern records a known failure fingerprint and its associated
// repair strategy. Part of C-5 structured failure pattern learning.
//
// Safety invariant (C-5): Fitness function definitions, weights, and
// thresholds are NOT subject to self-improvement. Only Planner/Worker
// prompts, configuration, and personas may be improved.
//
// Field invariants — F-011:
//   - SuccessCount and SuccessRate are derived metrics maintained together
//     by FingerprintDB.RecordOutcome (and equivalents). External callers
//     MUST NOT mutate either field independently; doing so leaves the pair
//     inconsistent (e.g. SuccessRate=1.0 with SuccessCount=0 misleads the
//     evolution scoring path). Treat both fields as read-only outside the
//     learnings package and update via the package's own methods only.
type FailurePattern struct {
	Fingerprint     string    `json:"fingerprint"`
	ErrorCategory   string    `json:"error_category"`
	RepairStrategy  string    `json:"repair_strategy"`
	OccurrenceCount int       `json:"occurrence_count"`
	LastSeen        time.Time `json:"last_seen"`
	SuccessRate     float64   `json:"success_rate"`
	SuccessCount    int       `json:"success_count"`
}

// FingerprintDB is an in-memory store of failure patterns keyed by fingerprint.
// It enforces a maximum size and evicts the oldest entry when full.
type FingerprintDB struct {
	patterns    map[string]*FailurePattern
	maxSize     int
	mu          sync.RWMutex
	oldestKey   string    // cached oldest entry key for O(1) eviction
	oldestTime  time.Time // cached oldest LastSeen timestamp
	oldestValid bool      // whether the cached oldest entry is up-to-date
}

// NewFingerprintDB creates an empty FingerprintDB with the given maximum capacity.
func NewFingerprintDB(maxSize int) *FingerprintDB {
	if maxSize <= 0 {
		maxSize = 1000
	}
	return &FingerprintDB{
		patterns: make(map[string]*FailurePattern),
		maxSize:  maxSize,
	}
}

// Store records or updates a failure pattern. If the fingerprint already exists,
// OccurrenceCount is incremented and LastSeen is updated. If the DB is at
// capacity, the oldest entry (by LastSeen) is evicted before inserting.
func (db *FingerprintDB) Store(fp string, category string, strategy string) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if existing, ok := db.patterns[fp]; ok {
		existing.OccurrenceCount++
		existing.LastSeen = time.Now()
		if category != "" {
			existing.ErrorCategory = category
		}
		if strategy != "" {
			existing.RepairStrategy = strategy
		}
		// Recalculate SuccessRate to stay consistent after OccurrenceCount change.
		if existing.OccurrenceCount > 0 {
			existing.SuccessRate = float64(existing.SuccessCount) / float64(existing.OccurrenceCount)
		}
		if fp == db.oldestKey {
			db.oldestValid = false
		}
		return
	}

	// Evict oldest if at capacity.
	if len(db.patterns) >= db.maxSize {
		db.evictOldest()
	}

	now := time.Now()
	db.patterns[fp] = &FailurePattern{
		Fingerprint:     fp,
		ErrorCategory:   category,
		RepairStrategy:  strategy,
		OccurrenceCount: 1,
		LastSeen:        now,
	}
	if !db.oldestValid || now.Before(db.oldestTime) {
		db.oldestKey = fp
		db.oldestTime = now
		db.oldestValid = true
	}
}

// Query looks up a failure pattern by exact fingerprint.
func (db *FingerprintDB) Query(fp string) (*FailurePattern, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	p, ok := db.patterns[fp]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// FindSimilar returns up to maxResults patterns whose fingerprint shares a
// common prefix with fp. This is a placeholder for future similarity search
// (e.g., Hamming distance).
//
// Performance: O(n) scan is acceptable here because the FingerprintDB is
// bounded by maxSize (default 1000) and prefix overlap requires per-entry
// comparison. A trie or embedding index would be needed for sub-linear lookup.
func (db *FingerprintDB) FindSimilar(fp string, maxResults int) []FailurePattern {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var results []FailurePattern
	for _, p := range db.patterns {
		if p.Fingerprint == fp {
			continue
		}
		if hasPrefixOverlap(fp, p.Fingerprint) {
			results = append(results, *p)
			if len(results) >= maxResults {
				break
			}
		}
	}
	return results
}

// RecordSuccess increments the success count for a fingerprint and recomputes
// SuccessRate as successCount / OccurrenceCount.
// Both fields are read and written under db.mu (write lock) to ensure consistency
// with Store(), which may concurrently increment OccurrenceCount.
func (db *FingerprintDB) RecordSuccess(fp string) {
	db.mu.Lock()
	defer db.mu.Unlock()

	p, ok := db.patterns[fp]
	if !ok {
		return
	}
	p.SuccessCount++
	occurrences := p.OccurrenceCount
	if occurrences > 0 {
		p.SuccessRate = float64(p.SuccessCount) / float64(occurrences)
	}
}

// SuggestStrategy returns the recorded repair strategy for a fingerprint,
// but only if there has been at least one successful repair (SuccessRate > 0).
func (db *FingerprintDB) SuggestStrategy(fp string) (string, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	p, ok := db.patterns[fp]
	if !ok {
		return "", false
	}
	if p.SuccessRate <= 0 {
		return "", false
	}
	return p.RepairStrategy, true
}

// Size returns the current number of stored patterns.
func (db *FingerprintDB) Size() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.patterns)
}

// Patterns returns a snapshot of all stored failure patterns.
func (db *FingerprintDB) Patterns() []FailurePattern {
	db.mu.RLock()
	defer db.mu.RUnlock()

	result := make([]FailurePattern, 0, len(db.patterns))
	for _, p := range db.patterns {
		result = append(result, *p)
	}
	return result
}

// SaveJSON persists the DB snapshot to path. The write is an atomic rename so
// daemon shutdown cannot leave a partially-written learning file behind.
func (db *FingerprintDB) SaveJSON(path string) error {
	db.mu.RLock()
	patterns := make([]FailurePattern, 0, len(db.patterns))
	for _, p := range db.patterns {
		patterns = append(patterns, *p)
	}
	db.mu.RUnlock()

	sort.Slice(patterns, func(i, j int) bool {
		return patterns[i].Fingerprint < patterns[j].Fingerprint
	})

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(patterns, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadFingerprintDB loads a previously saved DB. A missing file is treated as
// an empty DB so first-run startup needs no special handling.
func LoadFingerprintDB(path string, maxSize int) (*FingerprintDB, error) {
	db := NewFingerprintDB(maxSize)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return db, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return db, nil
	}
	var patterns []FailurePattern
	if err := json.Unmarshal(data, &patterns); err != nil {
		return nil, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range patterns {
		p := patterns[i]
		if p.Fingerprint == "" {
			continue
		}
		if p.OccurrenceCount <= 0 {
			p.OccurrenceCount = 1
		}
		if p.SuccessCount == 0 && p.SuccessRate > 0 {
			p.SuccessCount = int(p.SuccessRate*float64(p.OccurrenceCount) + 0.5)
		}
		if p.OccurrenceCount > 0 {
			p.SuccessRate = float64(p.SuccessCount) / float64(p.OccurrenceCount)
		}
		cp := p
		db.patterns[p.Fingerprint] = &cp
		if len(db.patterns) > db.maxSize {
			db.evictOldest()
		}
	}
	db.rebuildOldestLocked()
	return db, nil
}

// evictOldest removes the entry with the oldest LastSeen timestamp.
// Caller must hold db.mu write lock.
func (db *FingerprintDB) evictOldest() {
	if db.oldestValid {
		delete(db.patterns, db.oldestKey)
		db.oldestValid = false
		return
	}
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, p := range db.patterns {
		if first || p.LastSeen.Before(oldestTime) {
			oldestKey = k
			oldestTime = p.LastSeen
			first = false
		}
	}
	if oldestKey != "" {
		delete(db.patterns, oldestKey)
	}
}

func (db *FingerprintDB) rebuildOldestLocked() {
	db.oldestKey = ""
	db.oldestTime = time.Time{}
	db.oldestValid = false
	for k, p := range db.patterns {
		if !db.oldestValid || p.LastSeen.Before(db.oldestTime) {
			db.oldestKey = k
			db.oldestTime = p.LastSeen
			db.oldestValid = true
		}
	}
}

// hasPrefixOverlap checks whether two strings share a common prefix of at
// least half the length of the shorter string. This is a placeholder heuristic
// for future similarity algorithms (e.g., edit distance or embedding-based).
//
// The 50% threshold balances false-positive avoidance with catching obvious
// variants of the same error (e.g., different stack traces for the same root
// cause). The check is bidirectional: either string's prefix may match.
//
// Complexity: O(min(len(a), len(b))) per call, acceptable given FingerprintDB's
// bounded size (maxSize, default 1000).
func hasPrefixOverlap(a, b string) bool {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	if minLen == 0 {
		return false
	}
	threshold := minLen / 2
	if threshold == 0 {
		threshold = 1
	}
	return strings.HasPrefix(b, a[:threshold]) || strings.HasPrefix(a, b[:threshold])
}

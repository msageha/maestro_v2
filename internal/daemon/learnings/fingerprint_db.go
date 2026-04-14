package learnings

import (
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
type FailurePattern struct {
	Fingerprint     string
	ErrorCategory   string
	RepairStrategy  string
	OccurrenceCount int
	LastSeen        time.Time
	SuccessRate     float64

	// successCount tracks successful repairs for SuccessRate computation.
	successCount int
}

// FingerprintDB is an in-memory store of failure patterns keyed by fingerprint.
// It enforces a maximum size and evicts the oldest entry when full.
type FingerprintDB struct {
	patterns map[string]*FailurePattern
	maxSize  int
	mu       sync.RWMutex
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
		return
	}

	// Evict oldest if at capacity.
	if len(db.patterns) >= db.maxSize {
		db.evictOldest()
	}

	db.patterns[fp] = &FailurePattern{
		Fingerprint:     fp,
		ErrorCategory:   category,
		RepairStrategy:  strategy,
		OccurrenceCount: 1,
		LastSeen:        time.Now(),
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
	p.successCount++
	occurrences := p.OccurrenceCount
	if occurrences > 0 {
		p.SuccessRate = float64(p.successCount) / float64(occurrences)
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

// evictOldest removes the entry with the oldest LastSeen timestamp.
// Caller must hold db.mu write lock.
func (db *FingerprintDB) evictOldest() {
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

// hasPrefixOverlap checks whether two strings share a common prefix of at
// least half the length of the shorter string. This is a simple heuristic
// placeholder for future similarity algorithms.
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

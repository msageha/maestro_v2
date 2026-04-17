package dispatch

import (
	"sort"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// SortableEntry wraps a queue entry with computed effective priority for sorting.
type SortableEntry struct {
	Index             int
	EffectivePriority int
	CreatedAt         time.Time
	ID                string
}

// SortKey holds the fields extracted from a queue entry for pending-sort filtering.
type SortKey struct {
	Status    model.Status
	Priority  int
	CreatedAt string
	ID        string
}

// EffectivePriority computes the aging-adjusted priority.
// effective_priority = max(0, priority - floor(age_seconds / priority_aging_sec))
//
// Overflow prevention strategy:
//   - Input clamping: negative priority is clamped to 0; non-positive priorityAgingSec
//     disables aging entirely.
//   - Duration overflow guard: priorityAgingSec > 2^53-1 is rejected to prevent
//     precision loss in the Duration conversion (int64 nanoseconds).
//   - Output clamping: the result is always in [0, priority], so it cannot exceed
//     the input priority or go negative.
//   - 32-bit safety: aging is derived from int64(age/interval) and compared against
//     int64(priority) before the int() cast, guaranteeing no truncation on 32-bit
//     platforms where int is 32 bits.
//
// L-6: Uses integer duration arithmetic instead of float64 to avoid overflow on
// 32-bit systems and float rounding issues with extreme age values.
func EffectivePriority(priority int, createdAt string, priorityAgingSec int) int {
	if priorityAgingSec <= 0 || priority <= 0 {
		return max(priority, 0)
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return clampPriority(priority)
	}
	age := time.Since(created)
	if age <= 0 {
		// Future createdAt — no aging applied
		return clampPriority(priority)
	}
	// Guard against Duration overflow for very large priorityAgingSec values.
	// time.Duration is int64 nanoseconds; max safe seconds ≈ 9.2e9 (~292 years).
	// Why: 2^53-1 is the largest integer exactly representable in float64,
	// ensuring no precision loss when converting to time.Duration nanoseconds.
	const maxSafeSec = 1<<53 - 1
	if priorityAgingSec > maxSafeSec {
		return clampPriority(priority)
	}
	interval := time.Duration(priorityAgingSec) * time.Second
	// Integer division: equivalent to floor(age_seconds / priorityAgingSec)
	aging := int64(age / interval)
	if aging >= int64(priority) {
		return 0
	}
	return clampPriority(priority - int(aging))
}

// clampPriority ensures a priority value stays within [0, math.MaxInt].
func clampPriority(p int) int {
	if p < 0 {
		return 0
	}
	return p
}

// SortPendingIndices filters pending items and returns their original indices
// sorted by effective_priority ASC → created_at ASC → id ASC.
func SortPendingIndices[T any](items []T, extract func(T) SortKey, priorityAgingSec int) []int {
	entries := make([]SortableEntry, 0, len(items))
	for i, item := range items {
		key := extract(item)
		if key.Status != model.StatusPending {
			continue
		}
		created, _ := time.Parse(time.RFC3339, key.CreatedAt)
		entries = append(entries, SortableEntry{
			Index:             i,
			EffectivePriority: EffectivePriority(key.Priority, key.CreatedAt, priorityAgingSec),
			CreatedAt:         created,
			ID:                key.ID,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].EffectivePriority != entries[j].EffectivePriority {
			return entries[i].EffectivePriority < entries[j].EffectivePriority
		}
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})

	indices := make([]int, len(entries))
	for i, e := range entries {
		indices[i] = e.Index
	}
	return indices
}

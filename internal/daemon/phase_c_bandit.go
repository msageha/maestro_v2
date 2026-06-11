package daemon

// C-2 (adaptive model selection) dispatch→result context handoff. The
// contextual bandit needs the task's Bloom level at reward time, but
// model.TaskResult does not carry one, so — mirroring the C-4 taskDecisions
// pattern — the level is recorded in-memory at dispatch and consumed when
// the result arrives. A daemon restart between dispatch and result loses the
// record; the reward then degrades to a global-only update (bloom 0).

// taskBloomMaxEntries bounds the dispatch-time taskID→bloom map. Entries
// are normally consumed when the task's result arrives, but cancelled /
// superseded tasks never produce a reward, so a long-running daemon would
// leak entries without a cap. 4096 entries is far beyond any realistic
// number of concurrently in-flight tasks.
const taskBloomMaxEntries = 4096

// taskBloomEntry is a recorded Bloom level tagged with the generation it
// was recorded in. The seq lets FIFO eviction distinguish a stale order
// entry (left behind by consume → re-record of the same taskID) from the
// live record: deleting on taskID alone would let the stale entry evict the
// newer generation.
type taskBloomEntry struct {
	level int
	seq   uint64
}

// taskBloomOrderEntry is an insertion-order record for FIFO eviction.
type taskBloomOrderEntry struct {
	id  string
	seq uint64
}

// RecordTaskBloom remembers the Bloom level of a dispatched task so the C-2
// reward path can route the eventual reward to the matching difficulty
// bucket. No-op when the bandit is disabled, the taskID is empty, or the
// level maps to no bucket (see bloomBucket). nil-safe.
//
// Eviction is FIFO with lazy deletion: consumed entries linger in
// taskBloomOrder until eviction or compaction skips over them (matched by
// id+seq, so only the exact recorded generation is ever evicted).
func (m *PhaseCManager) RecordTaskBloom(taskID string, bloomLevel int) {
	if m == nil || m.BanditSelector == nil || taskID == "" {
		return
	}
	if bloomBucket(bloomLevel) == bloomBucketInvalid {
		return
	}
	m.banditMu.Lock()
	defer m.banditMu.Unlock()
	if existing, exists := m.taskBloom[taskID]; exists {
		// Re-dispatch of a live entry: refresh the level, keep the
		// generation and order position.
		existing.level = bloomLevel
		m.taskBloom[taskID] = existing
		return
	}
	for len(m.taskBloom) >= taskBloomMaxEntries && len(m.taskBloomOrder) > 0 {
		oldest := m.taskBloomOrder[0]
		m.taskBloomOrder = m.taskBloomOrder[1:]
		if e, ok := m.taskBloom[oldest.id]; ok && e.seq == oldest.seq {
			delete(m.taskBloom, oldest.id)
		}
	}
	// Compact the order slice when consumed (stale) IDs dominate it;
	// without this, steady consume traffic grows the slice unboundedly
	// because FIFO eviction never triggers while the map stays small.
	if len(m.taskBloomOrder) >= taskBloomMaxEntries*2 {
		live := m.taskBloomOrder[:0]
		for _, oe := range m.taskBloomOrder {
			if e, ok := m.taskBloom[oe.id]; ok && e.seq == oe.seq {
				live = append(live, oe)
			}
		}
		m.taskBloomOrder = live
	}
	m.taskBloomSeq++
	m.taskBloom[taskID] = taskBloomEntry{level: bloomLevel, seq: m.taskBloomSeq}
	m.taskBloomOrder = append(m.taskBloomOrder, taskBloomOrderEntry{id: taskID, seq: m.taskBloomSeq})
}

// ConsumeTaskBloom returns the Bloom level recorded for taskID at dispatch
// and removes the entry. Returns 0 when unknown (never dispatched in this
// daemon process, evicted, or bandit disabled); callers treat 0 as "no
// bucket" and record the reward globally only. nil-safe.
func (m *PhaseCManager) ConsumeTaskBloom(taskID string) int {
	if m == nil || taskID == "" {
		return 0
	}
	m.banditMu.Lock()
	defer m.banditMu.Unlock()
	e, ok := m.taskBloom[taskID]
	if !ok {
		return 0
	}
	delete(m.taskBloom, taskID)
	return e.level
}

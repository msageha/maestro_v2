package daemon

import (
	"crypto/sha256"
	"os"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

// selfWriteTracker tracks files written by the daemon to filter fsnotify self-notifications.
// Uses content hashing (SHA-256) instead of TTL to reliably detect self-written files.
// When the daemon writes a YAML file via UDS handler, it records the content hash here.
// fsnotifyLoop checks this tracker by reading the file and comparing hashes.
type selfWriteTracker struct {
	mu     sync.Mutex              // protects stamps
	stamps map[string]writeStamp
}

// writeStamp stores a content hash and a deadline for stale-entry cleanup.
type writeStamp struct {
	Hash     [sha256.Size]byte
	Deadline time.Time
}

func newSelfWriteTracker() *selfWriteTracker {
	return &selfWriteTracker{stamps: make(map[string]writeStamp)}
}

// Record stores a content hash for a self-written file.
// The data is marshaled to YAML to compute the hash (matching AtomicWrite's output).
func (t *selfWriteTracker) Record(path string, data any) {
	content, err := yamlv3.Marshal(data)
	if err != nil {
		return // best-effort
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stamps[path] = writeStamp{
		Hash:     sha256.Sum256(content),
		Deadline: time.Now().Add(30 * time.Second),
	}
	t.cleanStaleLocked()
}

// Consume checks if a file's current content matches a recorded self-write hash.
// If so, removes it from tracking and returns true.
//
// M-08: Uses a three-phase snapshot/revalidate/consume pattern to mitigate TOCTOU:
//   Phase 1: Snapshot the stamp under lock.
//   Phase 2: Read file content outside lock (I/O must not hold the lock).
//   Phase 3: Re-acquire lock, revalidate that the stamp hasn't been replaced
//            or consumed by another goroutine, then consume only if the hash matches.
// This ensures that a concurrent Record() between Phase 1 and Phase 3 does not
// cause the new stamp to be incorrectly consumed.
func (t *selfWriteTracker) Consume(path string) bool {
	// Phase 1: Snapshot stamp under lock
	t.mu.Lock()
	stamp, ok := t.stamps[path]
	if !ok {
		t.cleanStaleLocked()
		t.mu.Unlock()
		return false
	}
	if time.Now().After(stamp.Deadline) {
		delete(t.stamps, path)
		t.cleanStaleLocked()
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()

	// Phase 2: Read file outside lock (I/O)
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fileHash := sha256.Sum256(content)

	// Phase 3: Revalidate and consume under lock
	t.mu.Lock()
	defer t.mu.Unlock()

	cur, ok := t.stamps[path]
	if !ok || cur != stamp {
		// Stamp was consumed by another goroutine or replaced by Record
		t.cleanStaleLocked()
		return false
	}
	if time.Now().After(cur.Deadline) {
		delete(t.stamps, path)
		t.cleanStaleLocked()
		return false
	}
	if fileHash != cur.Hash {
		t.cleanStaleLocked()
		return false
	}

	delete(t.stamps, path)
	t.cleanStaleLocked()
	return true
}

// Len returns the number of tracked paths (for testing).
func (t *selfWriteTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.stamps)
}

// cleanStaleLocked removes entries past their deadline. Must be called with mu held.
func (t *selfWriteTracker) cleanStaleLocked() {
	now := time.Now()
	for p, s := range t.stamps {
		if now.After(s.Deadline) {
			delete(t.stamps, p)
		}
	}
}

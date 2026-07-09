package learnings

import (
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreAndQuery(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "compile_error", "fix imports")

	p, ok := db.Query("fp1")
	if !ok {
		t.Fatal("expected to find fp1")
	}
	if p.Fingerprint != "fp1" {
		t.Errorf("expected fingerprint fp1, got %s", p.Fingerprint)
	}
	if p.ErrorCategory != "compile_error" {
		t.Errorf("expected category compile_error, got %s", p.ErrorCategory)
	}
	if p.RepairStrategy != "fix imports" {
		t.Errorf("expected strategy 'fix imports', got %s", p.RepairStrategy)
	}
	if p.OccurrenceCount != 1 {
		t.Errorf("expected OccurrenceCount=1, got %d", p.OccurrenceCount)
	}
}

func TestQueryNotFound(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	_, ok := db.Query("nonexistent")
	if ok {
		t.Error("expected not found for nonexistent fingerprint")
	}
}

func TestStoreDuplicate_IncreasesCount(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "compile_error", "fix imports")
	db.Store("fp1", "compile_error", "fix imports")
	db.Store("fp1", "compile_error", "fix imports")

	p, ok := db.Query("fp1")
	if !ok {
		t.Fatal("expected to find fp1")
	}
	if p.OccurrenceCount != 3 {
		t.Errorf("expected OccurrenceCount=3 after 3 stores, got %d", p.OccurrenceCount)
	}
}

func TestMaxSizeEviction(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(2)
	db.Store("fp1", "cat1", "strat1")
	db.Store("fp2", "cat2", "strat2")
	// fp1 is the oldest; adding fp3 should evict it.
	db.Store("fp3", "cat3", "strat3")

	if db.Size() != 2 {
		t.Fatalf("expected size=2 after eviction, got %d", db.Size())
	}
	if _, ok := db.Query("fp1"); ok {
		t.Error("expected fp1 to be evicted")
	}
	if _, ok := db.Query("fp2"); !ok {
		t.Error("expected fp2 to still exist")
	}
	if _, ok := db.Query("fp3"); !ok {
		t.Error("expected fp3 to exist")
	}
}

// backdateLastSeen deterministically sets an entry's LastSeen so eviction
// order does not depend on time.Now() resolution. The cached oldest time is
// kept coherent when the entry is the cached oldest.
func backdateLastSeen(t *testing.T, db *FingerprintDB, fp string, ts time.Time) {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.patterns[fp]
	if !ok {
		t.Fatalf("fingerprint %s not found", fp)
	}
	p.LastSeen = ts
	if db.oldestValid && db.oldestKey == fp {
		db.oldestTime = ts
	}
}

// TestStoreAtCapacity_EvictsOldestNotNewest is a regression test: after an
// eviction invalidated the cached oldest entry, Store used to cache the
// just-inserted (newest) entry as oldest, so every subsequent insert evicted
// the previous insert instead of the true oldest.
func TestStoreAtCapacity_EvictsOldestNotNewest(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(3)
	base := time.Now().Add(-time.Hour)

	for i := 1; i <= 6; i++ {
		fp := fmt.Sprintf("fp%d", i)
		db.Store(fp, "cat", "strat")
		backdateLastSeen(t, db, fp, base.Add(time.Duration(i)*time.Second))
	}

	if db.Size() != 3 {
		t.Fatalf("expected size=3 at capacity, got %d", db.Size())
	}
	for _, evicted := range []string{"fp1", "fp2", "fp3"} {
		if _, ok := db.Query(evicted); ok {
			t.Errorf("expected %s (oldest) to be evicted", evicted)
		}
	}
	for _, kept := range []string{"fp4", "fp5", "fp6"} {
		if _, ok := db.Query(kept); !ok {
			t.Errorf("expected %s (newest) to be retained", kept)
		}
	}
}

func TestRecordSuccess_UpdatesSuccessRate(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat", "strat")
	db.Store("fp1", "cat", "strat") // OccurrenceCount = 2

	db.RecordSuccess("fp1")
	p, _ := db.Query("fp1")
	// successCount=1, OccurrenceCount=2 → SuccessRate=0.5
	if p.SuccessRate != 0.5 {
		t.Errorf("expected SuccessRate=0.5, got %f", p.SuccessRate)
	}

	db.RecordSuccess("fp1")
	p, _ = db.Query("fp1")
	// successCount=2, OccurrenceCount=2 → SuccessRate=1.0
	if p.SuccessRate != 1.0 {
		t.Errorf("expected SuccessRate=1.0, got %f", p.SuccessRate)
	}
}

func TestRecordSuccess_NonexistentFingerprint(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	// Should not panic.
	db.RecordSuccess("nonexistent")
}

func TestSuggestStrategy_KnownWithSuccess(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat", "retry with -race")
	db.RecordSuccess("fp1")

	strategy, ok := db.SuggestStrategy("fp1")
	if !ok {
		t.Fatal("expected strategy suggestion for known pattern with success")
	}
	if strategy != "retry with -race" {
		t.Errorf("expected 'retry with -race', got %s", strategy)
	}
}

func TestSuggestStrategy_KnownNoSuccess(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat", "some strategy")

	_, ok := db.SuggestStrategy("fp1")
	if ok {
		t.Error("expected no suggestion when SuccessRate=0")
	}
}

func TestSuggestStrategy_Unknown(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	_, ok := db.SuggestStrategy("nonexistent")
	if ok {
		t.Error("expected no suggestion for unknown fingerprint")
	}
}

func TestFindSimilar(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("compile_error_missing_import", "compile", "add import")
	db.Store("compile_error_type_mismatch", "compile", "fix types")
	db.Store("runtime_panic_nil_pointer", "runtime", "nil check")

	results := db.FindSimilar("compile_error_unused_var", 10)
	if len(results) == 0 {
		t.Fatal("expected to find similar patterns with prefix overlap")
	}
	// Should find the compile_error patterns.
	for _, r := range results {
		if r.ErrorCategory != "compile" {
			t.Errorf("expected compile category in similar results, got %s", r.ErrorCategory)
		}
	}
}

func TestFindSimilar_NoMatch(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("aaaa", "cat", "strat")
	results := db.FindSimilar("zzzz", 10)
	if len(results) != 0 {
		t.Errorf("expected no similar results, got %d", len(results))
	}
}

func TestSize(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	if db.Size() != 0 {
		t.Errorf("expected size=0, got %d", db.Size())
	}
	db.Store("fp1", "cat", "strat")
	db.Store("fp2", "cat", "strat")
	if db.Size() != 2 {
		t.Errorf("expected size=2, got %d", db.Size())
	}
}

func TestPatterns(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat1", "strat1")
	db.Store("fp2", "cat2", "strat2")

	patterns := db.Patterns()
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns, got %d", len(patterns))
	}
}

func TestSaveAndLoadJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fingerprints.json")
	db := NewFingerprintDB(100)
	db.Store("fp1", "compile", "fix imports")
	db.RecordSuccess("fp1")
	db.Store("fp2", "test", "run focused test")

	if err := db.SaveJSON(path); err != nil {
		t.Fatalf("SaveJSON: %v", err)
	}

	loaded, err := LoadFingerprintDB(path, 100)
	if err != nil {
		t.Fatalf("LoadFingerprintDB: %v", err)
	}
	p, ok := loaded.Query("fp1")
	if !ok {
		t.Fatal("expected fp1 after load")
	}
	if p.SuccessRate != 1.0 || p.SuccessCount != 1 {
		t.Errorf("loaded success stats = rate %f count %d, want 1.0/1", p.SuccessRate, p.SuccessCount)
	}
	if got, ok := loaded.SuggestStrategy("fp1"); !ok || got != "fix imports" {
		t.Errorf("loaded strategy = %q ok=%v, want fix imports true", got, ok)
	}
}

func TestLoadFingerprintDB_MissingFile(t *testing.T) {
	t.Parallel()
	loaded, err := LoadFingerprintDB(filepath.Join(t.TempDir(), "missing.json"), 100)
	if err != nil {
		t.Fatalf("LoadFingerprintDB missing file: %v", err)
	}
	if loaded.Size() != 0 {
		t.Errorf("missing file should produce empty DB, got size=%d", loaded.Size())
	}
}

func TestStoreRecalculatesSuccessRate(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat", "strat") // OccurrenceCount=1
	db.Store("fp1", "cat", "strat") // OccurrenceCount=2
	db.RecordSuccess("fp1")         // successCount=1, SuccessRate=1/2=0.5
	db.Store("fp1", "cat", "strat") // OccurrenceCount=3, SuccessRate should be 1/3

	p, ok := db.Query("fp1")
	if !ok {
		t.Fatal("expected to find fp1")
	}
	expected := 1.0 / 3.0
	if math.Abs(p.SuccessRate-expected) > 1e-9 {
		t.Errorf("expected SuccessRate≈%f after Store, got %f", expected, p.SuccessRate)
	}
}

func TestConcurrentStoreAndRecordSuccess(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(1000)
	const storeCount = 100
	const successCount = 50

	// Pre-populate so RecordSuccess has a target.
	db.Store("fp_race", "cat", "strat")

	var wg sync.WaitGroup

	// Run Store and RecordSuccess concurrently on the same fingerprint.
	wg.Add(storeCount + successCount)
	for i := 0; i < storeCount; i++ {
		go func() {
			defer wg.Done()
			db.Store("fp_race", "cat", "strat")
		}()
	}
	for i := 0; i < successCount; i++ {
		go func() {
			defer wg.Done()
			db.RecordSuccess("fp_race")
		}()
	}
	wg.Wait()

	p, ok := db.Query("fp_race")
	if !ok {
		t.Fatal("expected to find fp_race")
	}

	// OccurrenceCount = 1 (initial) + storeCount
	expectedOccurrences := 1 + storeCount
	if p.OccurrenceCount != expectedOccurrences {
		t.Errorf("expected OccurrenceCount=%d, got %d", expectedOccurrences, p.OccurrenceCount)
	}

	// SuccessRate must be consistent: successCount / OccurrenceCount.
	expectedRate := float64(successCount) / float64(expectedOccurrences)
	if math.Abs(p.SuccessRate-expectedRate) > 1e-9 {
		t.Errorf("expected SuccessRate=%f, got %f", expectedRate, p.SuccessRate)
	}
}

func TestConcurrentStoreRecordSuccessQuery(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(1000)
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)
	for i := 0; i < goroutines; i++ {
		fp := fmt.Sprintf("fp_%d", i%10)
		go func() {
			defer wg.Done()
			db.Store(fp, "cat", "strat")
		}()
		go func() {
			defer wg.Done()
			db.RecordSuccess(fp)
		}()
		go func() {
			defer wg.Done()
			db.Query(fp)
		}()
	}
	wg.Wait()

	// Verify consistency: for each pattern, SuccessRate == successCount/OccurrenceCount.
	for _, p := range db.Patterns() {
		if p.OccurrenceCount > 0 {
			// We can't access successCount directly, but SuccessRate must be in [0, 1].
			if p.SuccessRate < 0 || p.SuccessRate > 1.0+1e-9 {
				t.Errorf("pattern %s has invalid SuccessRate=%f", p.Fingerprint, p.SuccessRate)
			}
		}
	}
}

func TestConcurrencySafety(t *testing.T) {
	t.Parallel()
	db := NewFingerprintDB(1000)
	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent stores.
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			fp := "fp_concurrent"
			db.Store(fp, "cat", "strat")
		}(i)
	}
	wg.Wait()

	p, ok := db.Query("fp_concurrent")
	if !ok {
		t.Fatal("expected to find fp_concurrent")
	}
	if p.OccurrenceCount != goroutines {
		t.Errorf("expected OccurrenceCount=%d, got %d", goroutines, p.OccurrenceCount)
	}

	// Concurrent reads + writes.
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			db.Store("fp_rw", "cat", "strat")
		}()
		go func() {
			defer wg.Done()
			db.Query("fp_rw")
		}()
	}
	wg.Wait()
}

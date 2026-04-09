package learnings

import (
	"sync"
	"testing"
)

func TestStoreAndQuery(t *testing.T) {
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
	db := NewFingerprintDB(100)
	_, ok := db.Query("nonexistent")
	if ok {
		t.Error("expected not found for nonexistent fingerprint")
	}
}

func TestStoreDuplicate_IncreasesCount(t *testing.T) {
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

func TestRecordSuccess_UpdatesSuccessRate(t *testing.T) {
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
	db := NewFingerprintDB(100)
	// Should not panic.
	db.RecordSuccess("nonexistent")
}

func TestSuggestStrategy_KnownWithSuccess(t *testing.T) {
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
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat", "some strategy")

	_, ok := db.SuggestStrategy("fp1")
	if ok {
		t.Error("expected no suggestion when SuccessRate=0")
	}
}

func TestSuggestStrategy_Unknown(t *testing.T) {
	db := NewFingerprintDB(100)
	_, ok := db.SuggestStrategy("nonexistent")
	if ok {
		t.Error("expected no suggestion for unknown fingerprint")
	}
}

func TestFindSimilar(t *testing.T) {
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
	db := NewFingerprintDB(100)
	db.Store("aaaa", "cat", "strat")
	results := db.FindSimilar("zzzz", 10)
	if len(results) != 0 {
		t.Errorf("expected no similar results, got %d", len(results))
	}
}

func TestSize(t *testing.T) {
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
	db := NewFingerprintDB(100)
	db.Store("fp1", "cat1", "strat1")
	db.Store("fp2", "cat2", "strat2")

	patterns := db.Patterns()
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns, got %d", len(patterns))
	}
}

func TestConcurrencySafety(t *testing.T) {
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

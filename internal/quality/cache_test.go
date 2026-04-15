package quality

import (
	"sync"
	"testing"
	"time"
)

func TestNewResultCache(t *testing.T) {
	c := newResultCache(10, time.Minute)
	if c == nil {
		t.Fatal("newResultCache returned nil")
	}
	if c.Size() != 0 {
		t.Errorf("Size() = %d, want 0", c.Size())
	}
}

func TestResultCache_SetAndGet(t *testing.T) {
	c := newResultCache(10, time.Minute)
	key := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}
	val := &EvaluationResult{GateID: "g1", Passed: true, Action: ActionAllow}

	c.Set(key, val)

	got := c.Get(key)
	if got == nil {
		t.Fatal("Get returned nil for existing key")
	}
	if got.GateID != "g1" || !got.Passed || got.Action != ActionAllow {
		t.Errorf("Get = %+v, want GateID=g1 Passed=true Action=allow", got)
	}
}

func TestResultCache_GetMiss(t *testing.T) {
	c := newResultCache(10, time.Minute)
	key := &cacheKey{GateID: "missing", GateVersionHash: "v1", ContextFingerprint: "c1"}

	got := c.Get(key)
	if got != nil {
		t.Errorf("Get on missing key should return nil, got %+v", got)
	}
}

func TestResultCache_TTLExpiry(t *testing.T) {
	now := time.Now()
	c := newResultCache(10, 10*time.Millisecond)
	c.nowFunc = func() time.Time { return now }

	key := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}
	val := &EvaluationResult{GateID: "g1", Passed: true}

	c.Set(key, val)

	// Advance time past TTL
	now = now.Add(20 * time.Millisecond)

	got := c.Get(key)
	if got != nil {
		t.Errorf("Get should return nil for expired entry, got %+v", got)
	}
}

func TestResultCache_LRUEviction(t *testing.T) {
	c := newResultCache(3, time.Minute)

	for i := 0; i < 5; i++ {
		key := &cacheKey{GateID: "g", GateVersionHash: "v", ContextFingerprint: string(rune('a' + i))}
		val := &EvaluationResult{GateID: "g", Passed: true}
		c.Set(key, val)
	}

	if c.Size() != 3 {
		t.Errorf("Size() = %d, want 3 after LRU eviction", c.Size())
	}

	// Oldest entries (a, b) should be evicted; newest (c, d, e) should remain.
	evicted := &cacheKey{GateID: "g", GateVersionHash: "v", ContextFingerprint: "a"}
	if c.Get(evicted) != nil {
		t.Error("oldest entry 'a' should have been evicted")
	}

	newest := &cacheKey{GateID: "g", GateVersionHash: "v", ContextFingerprint: "e"}
	if c.Get(newest) == nil {
		t.Error("newest entry 'e' should still be present")
	}
}

func TestResultCache_UpdateExisting(t *testing.T) {
	c := newResultCache(10, time.Minute)
	key := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}

	c.Set(key, &EvaluationResult{GateID: "g1", Passed: false})
	c.Set(key, &EvaluationResult{GateID: "g1", Passed: true})

	got := c.Get(key)
	if got == nil || !got.Passed {
		t.Error("Set should update existing entry value")
	}
	if c.Size() != 1 {
		t.Errorf("Size() = %d, want 1 after updating existing key", c.Size())
	}
}

func TestResultCache_Clear(t *testing.T) {
	c := newResultCache(10, time.Minute)
	for i := 0; i < 5; i++ {
		key := &cacheKey{GateID: "g", GateVersionHash: "v", ContextFingerprint: string(rune('a' + i))}
		c.Set(key, &EvaluationResult{GateID: "g"})
	}

	c.Clear()

	if c.Size() != 0 {
		t.Errorf("Size() = %d after Clear(), want 0", c.Size())
	}
}

func TestResultCache_Stats(t *testing.T) {
	now := time.Now()
	c := newResultCache(10, 10*time.Millisecond)
	c.nowFunc = func() time.Time { return now }

	key1 := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}
	key2 := &cacheKey{GateID: "g2", GateVersionHash: "v2", ContextFingerprint: "c2"}

	c.Set(key1, &EvaluationResult{GateID: "g1"})
	c.Set(key2, &EvaluationResult{GateID: "g2"})

	stats := c.Stats()
	if stats.Size != 2 {
		t.Errorf("Stats.Size = %d, want 2", stats.Size)
	}
	if stats.MaxSize != 10 {
		t.Errorf("Stats.MaxSize = %d, want 10", stats.MaxSize)
	}
	if stats.Expired != 0 {
		t.Errorf("Stats.Expired = %d, want 0 (items not yet expired)", stats.Expired)
	}

	// Advance time past TTL
	now = now.Add(20 * time.Millisecond)

	stats = c.Stats()
	if stats.Expired != 2 {
		t.Errorf("Stats.Expired = %d, want 2 (items should be expired)", stats.Expired)
	}
}

func TestResultCache_GetReturnsCopy(t *testing.T) {
	c := newResultCache(10, time.Minute)
	key := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}
	orig := &EvaluationResult{GateID: "g1", Passed: true}

	c.Set(key, orig)

	got := c.Get(key)
	got.Passed = false // modify the copy

	got2 := c.Get(key)
	if !got2.Passed {
		t.Error("Get should return a copy; modifying it should not affect cached value")
	}
}

func TestResultCache_DeepCopySlices(t *testing.T) {
	c := newResultCache(10, time.Minute)
	key := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}
	orig := &EvaluationResult{
		GateID: "g1",
		Passed: false,
		FailedGates: []string{"gate1", "gate2"},
		RuleResults: []RuleResult{
			{RuleID: "r1", Passed: true, Message: "ok"},
			{RuleID: "r2", Passed: false, Message: "fail"},
		},
	}

	c.Set(key, orig)

	got := c.Get(key)

	// Mutate the returned slices
	got.FailedGates[0] = "mutated"
	got.RuleResults[0].Message = "mutated"

	// Verify the cache is not polluted
	got2 := c.Get(key)
	if got2.FailedGates[0] != "gate1" {
		t.Errorf("FailedGates[0] = %q, want %q; cache was polluted by slice mutation", got2.FailedGates[0], "gate1")
	}
	if got2.RuleResults[0].Message != "ok" {
		t.Errorf("RuleResults[0].Message = %q, want %q; cache was polluted by slice mutation", got2.RuleResults[0].Message, "ok")
	}
}

func TestResultCache_SetDeepCopiesInput(t *testing.T) {
	c := newResultCache(10, time.Minute)
	key := &cacheKey{GateID: "g1", GateVersionHash: "v1", ContextFingerprint: "c1"}
	orig := &EvaluationResult{
		GateID:      "g1",
		FailedGates: []string{"gate1"},
	}

	c.Set(key, orig)

	// Mutate the original after Set
	orig.FailedGates[0] = "mutated"

	got := c.Get(key)
	if got.FailedGates[0] != "gate1" {
		t.Errorf("FailedGates[0] = %q, want %q; Set should deep copy input", got.FailedGates[0], "gate1")
	}
}

func TestResultCache_ConcurrentAccess(t *testing.T) {
	c := newResultCache(100, time.Minute)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := &cacheKey{GateID: "g", GateVersionHash: "v", ContextFingerprint: string(rune('a' + i%26))}
			val := &EvaluationResult{GateID: "g", Passed: true}
			c.Set(key, val)
			c.Get(key)
			c.Stats()
			c.Size()
		}(i)
	}
	wg.Wait()

	if c.Size() < 0 {
		t.Error("cache in invalid state after concurrent access")
	}
}

func TestResultCache_KeyToString(t *testing.T) {
	c := newResultCache(10, time.Minute)
	tests := []struct {
		key  *cacheKey
		want string
	}{
		{&cacheKey{GateID: "a", GateVersionHash: "b", ContextFingerprint: "c"}, "a:b:c"},
		{&cacheKey{GateID: "", GateVersionHash: "", ContextFingerprint: ""}, "::"},
		{&cacheKey{GateID: "gate:1", GateVersionHash: "v2", ContextFingerprint: "fp3"}, "gate:1:v2:fp3"},
	}
	for _, tt := range tests {
		got := c.keyToString(tt.key)
		if got != tt.want {
			t.Errorf("keyToString(%+v) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

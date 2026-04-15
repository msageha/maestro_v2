package model

import (
	"sync"
	"testing"
	"time"
)

func TestNormalizeError_LineNumbers(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"colon_line", "error at file.go:42: something", "error at file.go something"},
		{"line_keyword", "error at line 42 in foo", "error at in foo"},
		{"multiple_lines", "file.go:10: and file.go:20: errors", "file.go and file.go errors"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeError(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeError(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeError_Timestamps(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"iso8601", "error 2024-01-15T10:30:00Z in handler", "error in handler"},
		{"iso8601_offset", "error 2024-01-15T10:30:00+09:00 in handler", "error in handler"},
		{"unix_timestamp", "error 1705312200 in handler", "error in handler"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeError(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeError(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeError_AbsPath(t *testing.T) {
	got := NormalizeError("open /home/user/project/src/main.go: no such file")
	want := "open main.go: no such file"
	if got != want {
		t.Errorf("abs path: got %q, want %q", got, want)
	}
}

func TestNormalizeError_MemoryAddress(t *testing.T) {
	got := NormalizeError("panic at 0x7fff5fbff8c0 in goroutine")
	want := "panic at addr in goroutine"
	if got != want {
		t.Errorf("mem addr: got %q, want %q", got, want)
	}
}

func TestNormalizeError_WhitespaceNormalization(t *testing.T) {
	got := NormalizeError("error   with\t\tmultiple   spaces")
	want := "error with multiple spaces"
	if got != want {
		t.Errorf("whitespace: got %q, want %q", got, want)
	}
}

func TestNormalizeError_Lowercase(t *testing.T) {
	got := NormalizeError("ERROR IN Handler")
	want := "error in handler"
	if got != want {
		t.Errorf("lowercase: got %q, want %q", got, want)
	}
}

func TestNormalizeError_Combined(t *testing.T) {
	input := "ERROR at /home/user/main.go:42: panic at 0xdeadbeef timestamp 2024-01-15T10:30:00Z"
	got := NormalizeError(input)
	want := "error at main.go panic at addr timestamp"
	// After normalization: timestamp removed first, then :42: removed, path abstracted, addr replaced, lowercased
	if got != want {
		t.Errorf("combined: got %q, want %q", got, want)
	}
}

func TestComputeFingerprint_SameError(t *testing.T) {
	fp1 := ComputeFingerprint("error in handler", "build", time.Now())
	fp2 := ComputeFingerprint("error in handler", "build", time.Now())
	if fp1.Hash != fp2.Hash {
		t.Errorf("same error should produce same hash: %s != %s", fp1.Hash, fp2.Hash)
	}
}

func TestComputeFingerprint_DifferentError(t *testing.T) {
	fp1 := ComputeFingerprint("error in handler", "build", time.Now())
	fp2 := ComputeFingerprint("error in parser", "build", time.Now())
	if fp1.Hash == fp2.Hash {
		t.Error("different errors should produce different hashes")
	}
}

func TestComputeFingerprint_DifferentCategory(t *testing.T) {
	fp1 := ComputeFingerprint("error in handler", "build", time.Now())
	fp2 := ComputeFingerprint("error in handler", "test", time.Now())
	if fp1.Hash == fp2.Hash {
		t.Error("same error with different category should produce different hashes")
	}
}

func TestComputeFingerprint_Fields(t *testing.T) {
	fp := ComputeFingerprint("normalized error text", "lint", time.Now())
	if fp.Category != "lint" {
		t.Errorf("Category = %q, want %q", fp.Category, "lint")
	}
	if fp.RawError != "normalized error text" {
		t.Errorf("RawError = %q, want %q", fp.RawError, "normalized error text")
	}
	if fp.Hash == "" {
		t.Error("Hash should not be empty")
	}
	if fp.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}

func TestComputeFingerprint_RawErrorTruncation(t *testing.T) {
	longError := make([]byte, 2000)
	for i := range longError {
		longError[i] = 'a'
	}
	fp := ComputeFingerprint(string(longError), "build", time.Now())
	if len(fp.RawError) != maxRawErrorLength {
		t.Errorf("RawError length = %d, want %d", len(fp.RawError), maxRawErrorLength)
	}
}

func TestComputeFingerprint_ClockParameter(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fp := ComputeFingerprint("error", "build", fixed)
	if !fp.CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want %v", fp.CreatedAt, fixed)
	}
}

func TestConsecutiveFailureTracker_Record(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(3)
	for i := 0; i < 5; i++ {
		tracker.Record(ComputeFingerprint("error", "build", time.Now()))
	}
	tracker.mu.Lock()
	count := len(tracker.fingerprints)
	tracker.mu.Unlock()
	if count != 3 {
		t.Errorf("history should be capped at 3, got %d", count)
	}
}

func TestConsecutiveFailureTracker_IsConsecutiveDuplicate(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(10)
	fp := ComputeFingerprint("same error", "build", time.Now())

	// Record 3 identical fingerprints
	for i := 0; i < 3; i++ {
		tracker.Record(fp)
	}

	if !tracker.IsConsecutiveDuplicate(3) {
		t.Error("3 identical fingerprints should be consecutive duplicate with threshold 3")
	}
	if tracker.IsConsecutiveDuplicate(4) {
		t.Error("only 3 recorded, threshold 4 should be false")
	}
}

func TestConsecutiveFailureTracker_IsConsecutiveDuplicate_Mixed(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(10)

	tracker.Record(ComputeFingerprint("error a", "build", time.Now()))
	tracker.Record(ComputeFingerprint("error b", "build", time.Now()))
	tracker.Record(ComputeFingerprint("error a", "build", time.Now()))

	if tracker.IsConsecutiveDuplicate(2) {
		t.Error("mixed fingerprints should not be consecutive duplicate")
	}
}

func TestConsecutiveFailureTracker_IsConsecutiveDuplicate_BreakThenConsecutive(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(10)

	tracker.Record(ComputeFingerprint("error a", "build", time.Now()))
	tracker.Record(ComputeFingerprint("error b", "build", time.Now()))
	tracker.Record(ComputeFingerprint("error b", "build", time.Now()))
	tracker.Record(ComputeFingerprint("error b", "build", time.Now()))

	if !tracker.IsConsecutiveDuplicate(3) {
		t.Error("last 3 are same, should be consecutive duplicate with threshold 3")
	}
	if tracker.IsConsecutiveDuplicate(4) {
		t.Error("last 4 are not all same, threshold 4 should be false")
	}
}

func TestConsecutiveFailureTracker_IsConsecutiveDuplicate_EdgeCases(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(10)

	// threshold 0
	if tracker.IsConsecutiveDuplicate(0) {
		t.Error("threshold 0 should return false")
	}
	// negative threshold
	if tracker.IsConsecutiveDuplicate(-1) {
		t.Error("negative threshold should return false")
	}
	// empty tracker
	if tracker.IsConsecutiveDuplicate(1) {
		t.Error("empty tracker should return false")
	}
}

func TestConsecutiveFailureTracker_LastFingerprint(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(10)

	if fp := tracker.LastFingerprint(); fp != nil {
		t.Error("empty tracker should return nil")
	}

	expected := ComputeFingerprint("last error", "test", time.Now())
	tracker.Record(ComputeFingerprint("first error", "build", time.Now()))
	tracker.Record(expected)

	got := tracker.LastFingerprint()
	if got == nil {
		t.Fatal("LastFingerprint should not be nil")
	}
	if got.Hash != expected.Hash {
		t.Errorf("LastFingerprint hash = %s, want %s", got.Hash, expected.Hash)
	}
}

func TestConsecutiveFailureTracker_Reset(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(10)
	tracker.Record(ComputeFingerprint("error", "build", time.Now()))
	tracker.Reset()

	if fp := tracker.LastFingerprint(); fp != nil {
		t.Error("after reset, LastFingerprint should be nil")
	}
	if tracker.IsConsecutiveDuplicate(1) {
		t.Error("after reset, IsConsecutiveDuplicate should be false")
	}
}

func TestConsecutiveFailureTracker_ThreadSafety(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(100)
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tracker.Record(ComputeFingerprint("error", "build", time.Now()))
			}
		}()
	}

	// Concurrent readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tracker.IsConsecutiveDuplicate(3)
				tracker.LastFingerprint()
			}
		}()
	}

	wg.Wait()
	// If we get here without a race condition panic, the test passes
}

func TestNewConsecutiveFailureTracker_DefaultMaxHistory(t *testing.T) {
	tracker := NewConsecutiveFailureTracker(0)
	if tracker.maxHistory != 10 {
		t.Errorf("default maxHistory = %d, want 10", tracker.maxHistory)
	}

	tracker = NewConsecutiveFailureTracker(-5)
	if tracker.maxHistory != 10 {
		t.Errorf("negative maxHistory = %d, want 10", tracker.maxHistory)
	}
}

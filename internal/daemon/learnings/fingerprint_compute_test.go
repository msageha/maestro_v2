package learnings

import "testing"

func TestComputeErrorFingerprint_StableAcrossVolatileFields(t *testing.T) {
	a := "2026-04-21T10:00:00Z worker1: operation failed at 0xabcdef: timeout after 30 seconds"
	b := "2025-01-01T15:30:00Z worker1: operation failed at 0x123456: timeout after 90 seconds"
	fpA, catA := ComputeErrorFingerprint(a)
	fpB, catB := ComputeErrorFingerprint(b)
	if fpA == "" || fpB == "" {
		t.Fatalf("expected non-empty fingerprints; got %q, %q", fpA, fpB)
	}
	if fpA != fpB {
		t.Errorf("fingerprints diverge on volatile fields: %q vs %q\nnormA=%q\nnormB=%q",
			fpA, fpB, NormalizeError(a), NormalizeError(b))
	}
	if catA != "timeout" || catB != "timeout" {
		t.Errorf("expected timeout category, got %q / %q", catA, catB)
	}
}

func TestComputeErrorFingerprint_EmptyInput(t *testing.T) {
	fp, cat := ComputeErrorFingerprint("")
	if fp != "" || cat != "" {
		t.Errorf("expected empty results for empty input, got fp=%q cat=%q", fp, cat)
	}
	fp, cat = ComputeErrorFingerprint("   ")
	if fp != "" || cat != "" {
		t.Errorf("expected empty results for whitespace input, got fp=%q cat=%q", fp, cat)
	}
}

func TestComputeErrorFingerprint_DifferentErrorsDiverge(t *testing.T) {
	fpTimeout, _ := ComputeErrorFingerprint("operation timeout after 10 seconds")
	fpPermission, _ := ComputeErrorFingerprint("operation failed: permission denied")
	if fpTimeout == "" || fpPermission == "" {
		t.Fatalf("unexpected empty fingerprint")
	}
	if fpTimeout == fpPermission {
		t.Errorf("expected different errors to have different fingerprints, both = %q", fpTimeout)
	}
}

func TestComputeErrorFingerprint_Categories(t *testing.T) {
	cases := []struct {
		msg      string
		expected string
	}{
		{"request timeout 30s", "timeout"},
		{"permission denied writing to /tmp/x", "permission"},
		{"file not found: main.go", "not_found"},
		{"syntax error at line 42", "syntax"},
		{"merge conflict in auth.go", "conflict"},
		{"network connection refused", "network"},
		{"test assertion failed for TestFoo", "test_failure"},
		{"compile error: undefined symbol", "build"},
		{"something unexpected occurred", "generic"},
	}
	for _, c := range cases {
		_, cat := ComputeErrorFingerprint(c.msg)
		if cat != c.expected {
			t.Errorf("classify(%q) = %q; want %q", c.msg, cat, c.expected)
		}
	}
}

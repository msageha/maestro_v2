package model

import "testing"

func TestComputeConflictGeneration_Deterministic(t *testing.T) {
	a := ComputeConflictGeneration("abc", "def", "phase1", "worker1")
	b := ComputeConflictGeneration("abc", "def", "phase1", "worker1")
	if a != b {
		t.Errorf("expected deterministic output, got %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("expected 16-char token, got %d (%q)", len(a), a)
	}
}

func TestComputeConflictGeneration_Unique(t *testing.T) {
	base := ComputeConflictGeneration("h1", "w1", "p1", "wk1")
	cases := []struct {
		name                                            string
		integ, worker, phase, wid string
	}{
		{"different integration HEAD", "h2", "w1", "p1", "wk1"},
		{"different worker branch SHA", "h1", "w2", "p1", "wk1"},
		{"different phase", "h1", "w1", "p2", "wk1"},
		{"different worker ID", "h1", "w1", "p1", "wk2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeConflictGeneration(c.integ, c.worker, c.phase, c.wid)
			if got == base {
				t.Errorf("expected distinct token from base, got same: %q", got)
			}
			if len(got) != 16 {
				t.Errorf("expected 16-char token, got %d", len(got))
			}
		})
	}
}

// Guard against trivial concatenation collisions: ("ab","c") vs ("a","bc")
// must yield distinct generations because the delimiter "|" disambiguates.
func TestComputeConflictGeneration_DelimiterSafe(t *testing.T) {
	g1 := ComputeConflictGeneration("ab", "c", "p", "w")
	g2 := ComputeConflictGeneration("a", "bc", "p", "w")
	if g1 == g2 {
		t.Errorf("delimiter must disambiguate concatenation, got equal: %q", g1)
	}
}

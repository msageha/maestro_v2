package agent

import (
	"context"
	"math"
	"os"
	"testing"
)

func floatEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestParseCPUTime(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    float64
		wantErr bool
	}{
		// Real macOS (BSD ps) output: "[[dd-]hh:]mm:ss.cc", verified against
		// this host's actual `ps -A -o pid=,ppid=,cputime=` output (e.g.
		// "37:34.27"). A parser that only handles whole-second fields
		// silently fails on every row on macOS -- see process_sampler.go.
		{"macos_mm_ss_frac", "37:34.27", 37*60 + 34.27, false},
		{"macos_zero_frac", "0:00.05", 0.05, false},
		{"macos_hh_mm_ss_frac", "1:02:03.50", 1*3600 + 2*60 + 3.50, false},
		// Linux (procps ps) output: "[dd-]hh:mm:ss", whole seconds only.
		{"linux_mm_ss", "05:30", 5*60 + 30, false},
		{"linux_hh_mm_ss", "01:02:03", 1*3600 + 2*60 + 3, false},
		{"linux_dd_hh_mm_ss", "2-00:00:00", 2 * 86400, false},
		{"zero", "00:00", 0, false},
		{"hh_mm_ss_zero_padded", "00:00:01", 1, false},
		{"garbage", "not-a-time", 0, true},
		{"too_many_parts", "1:2:3:4", 0, true},
		{"single_part", "30", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCPUTime(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCPUTime(%q): expected error, got nil", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCPUTime(%q): unexpected error: %v", tt.in, err)
			}
			if !floatEq(got, tt.want) {
				t.Errorf("parseCPUTime(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSumDescendantCPUSeconds(t *testing.T) {
	// Tree: 1 (shell, root) -> 2 (claude) -> 3 (sh -c make) -> 4 (cc1)
	//                                     -> 5 (unrelated sibling under claude)
	// Plus an unrelated process (100) with no relation to root, which must
	// not be counted.
	procs := []processInfo{
		{pid: 1, ppid: 0, cpuSeconds: 1},
		{pid: 2, ppid: 1, cpuSeconds: 2},
		{pid: 3, ppid: 2, cpuSeconds: 3},
		{pid: 4, ppid: 3, cpuSeconds: 40},
		{pid: 5, ppid: 2, cpuSeconds: 5},
		{pid: 100, ppid: 0, cpuSeconds: 999},
	}

	got := sumDescendantCPUSeconds(1, procs)
	want := 1.0 + 2 + 3 + 40 + 5
	if !floatEq(got, want) {
		t.Errorf("sumDescendantCPUSeconds(root=1) = %v, want %v", got, want)
	}
}

func TestSumDescendantCPUSeconds_RootNotInTable(t *testing.T) {
	// rootPID not present in the table at all (e.g. pane_pid read raced with
	// process exit): must not panic, sums whatever descendants exist under it.
	procs := []processInfo{
		{pid: 2, ppid: 1, cpuSeconds: 7},
	}
	got := sumDescendantCPUSeconds(1, procs)
	if !floatEq(got, 7) {
		t.Errorf("sumDescendantCPUSeconds(root=1, root missing) = %v, want 7", got)
	}
}

func TestSumDescendantCPUSeconds_CycleSafe(t *testing.T) {
	// Malformed/adversarial ppid cycle must not infinite-loop.
	procs := []processInfo{
		{pid: 1, ppid: 2, cpuSeconds: 1},
		{pid: 2, ppid: 1, cpuSeconds: 2},
	}
	got := sumDescendantCPUSeconds(1, procs)
	if !floatEq(got, 3) {
		t.Errorf("sumDescendantCPUSeconds with cycle = %v, want 3", got)
	}
}

// TestListAllProcesses_RealPS is a smoke test against the real `ps` binary
// on whatever platform the suite runs on. This exists specifically to catch
// the class of bug where `ps`'s actual output format silently diverges from
// what parseCPUTime/listAllProcesses assume (e.g. macOS's fractional-second
// cputime field): a systemic parse failure must surface as an error, not as
// a quietly-empty, error-free process list.
func TestListAllProcesses_RealPS(t *testing.T) {
	procs, err := listAllProcesses(context.Background())
	if err != nil {
		t.Fatalf("listAllProcesses: unexpected error (ps output format may have changed): %v", err)
	}
	if len(procs) == 0 {
		t.Fatal("listAllProcesses: expected at least one process (this test's own), got none")
	}

	self := os.Getpid()
	found := false
	for _, p := range procs {
		if p.pid == self {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("listAllProcesses: did not find this test process's own pid=%d in the output", self)
	}
}

func TestPSProcessSampler_RealPS_SelfHasNonNegativeCPUTime(t *testing.T) {
	// End-to-end smoke test of the production sampler against the real OS:
	// this process is always its own descendant (rootPID = its own pid), so
	// the result must be a valid non-negative reading, not an error.
	sampler := psProcessSampler{}
	got, err := sampler.descendantCPUSeconds(context.Background(), os.Getpid())
	if err != nil {
		t.Fatalf("descendantCPUSeconds(self): unexpected error: %v", err)
	}
	if got < 0 {
		t.Errorf("descendantCPUSeconds(self) = %v, want >= 0", got)
	}
}

package reconcile

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func newConfigWithLeaseSec(sec int) model.Config {
	return model.Config{
		Watcher: model.WatcherConfig{
			DispatchLeaseSec: sec,
		},
	}
}

func TestExtractWorkerID(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"valid worker1", "worker1.yaml", "worker1"},
		{"valid worker10", "worker10.yaml", "worker10"},
		{"planner.yaml", "planner.yaml", ""},
		{"no yaml suffix", "worker1.json", ""},
		{"no worker prefix", "agent1.yaml", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractWorkerID(tt.filename)
			if got != tt.want {
				t.Errorf("ExtractWorkerID(%q) = %q, want %q", tt.filename, got, tt.want)
			}
		})
	}
}

func TestRemoveFromSlice(t *testing.T) {
	tests := []struct {
		name   string
		input  []string
		target string
		want   int
	}{
		{"remove existing", []string{"a", "b", "c"}, "b", 2},
		{"remove non-existing", []string{"a", "b", "c"}, "d", 3},
		{"remove from empty", nil, "a", 0},
		{"remove duplicates", []string{"a", "b", "a", "c"}, "a", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RemoveFromSlice(tt.input, tt.target)
			if len(got) != tt.want {
				t.Errorf("RemoveFromSlice(%v, %q) len = %d, want %d", tt.input, tt.target, len(got), tt.want)
			}
			for _, v := range got {
				if v == tt.target {
					t.Errorf("RemoveFromSlice result still contains %q", tt.target)
				}
			}
		})
	}
}

func TestStuckThresholdSec(t *testing.T) {
	tests := []struct {
		name     string
		leaseSec int
		want     int
	}{
		{"default when zero", 0, 600},
		{"default when negative", -1, 600},
		{"custom value", 120, 240},
		{"standard 300", 300, 600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := newRun(&Deps{
				Config: newConfigWithLeaseSec(tt.leaseSec),
			})
			got := run.StuckThresholdSec()
			if got != tt.want {
				t.Errorf("StuckThresholdSec() = %d, want %d", got, tt.want)
			}
		})
	}
}

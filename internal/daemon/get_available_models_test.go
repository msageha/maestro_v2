package daemon

import (
	"sort"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestGetAvailableModels_ReturnsModelValuesNotWorkerKeys is a regression
// test for a bug where getAvailableModels iterated over wc.Models keys
// (worker IDs) instead of values (model names). The bandit selector was
// then primed with arms named "worker1", "worker2", ... — which never
// matched any real model and silently broke adaptive model selection.
func TestGetAvailableModels_ReturnsModelValuesNotWorkerKeys(t *testing.T) {
	cases := []struct {
		name     string
		cfg      model.WorkerConfig
		expected []string
	}{
		{
			name: "default_only",
			cfg: model.WorkerConfig{
				DefaultModel: "sonnet",
			},
			expected: []string{"sonnet"},
		},
		{
			name: "default_plus_overrides_dedup",
			cfg: model.WorkerConfig{
				DefaultModel: "sonnet",
				Models: map[string]string{
					"worker1": "opus",
					"worker2": "haiku",
					"worker3": "sonnet", // dedup against DefaultModel
				},
			},
			expected: []string{"haiku", "opus", "sonnet"},
		},
		{
			name: "no_default_only_overrides",
			cfg: model.WorkerConfig{
				Models: map[string]string{
					"worker1": "opus",
					"worker2": "haiku",
				},
			},
			expected: []string{"haiku", "opus"},
		},
		{
			name: "empty_value_skipped",
			cfg: model.WorkerConfig{
				DefaultModel: "sonnet",
				Models: map[string]string{
					"worker1": "",
					"worker2": "opus",
				},
			},
			expected: []string{"opus", "sonnet"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{config: model.Config{Agents: model.AgentsConfig{Workers: tc.cfg}}}
			got := d.getAvailableModels()

			// Verify worker IDs never leak into the output (regression).
			for id := range tc.cfg.Models {
				for _, m := range got {
					if m == id {
						t.Fatalf("worker ID %q leaked into model list %v", id, got)
					}
				}
			}

			sort.Strings(got)
			expected := append([]string{}, tc.expected...)
			sort.Strings(expected)
			if len(got) != len(expected) {
				t.Fatalf("len(models) = %d (%v), want %d (%v)", len(got), got, len(expected), expected)
			}
			for i := range got {
				if got[i] != expected[i] {
					t.Errorf("models[%d] = %q, want %q (full got=%v expected=%v)", i, got[i], expected[i], got, expected)
				}
			}
		})
	}
}

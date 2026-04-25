package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestHasPathOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{
			name: "exact_match",
			a:    "internal/daemon/",
			b:    "internal/daemon/",
			want: true,
		},
		{
			name: "exact_match_no_trailing_slash",
			a:    "internal/daemon",
			b:    "internal/daemon",
			want: true,
		},
		{
			name: "exact_match_mixed_trailing_slash",
			a:    "internal/daemon/",
			b:    "internal/daemon",
			want: true,
		},
		{
			name: "parent_contains_child",
			a:    "internal/daemon/",
			b:    "internal/daemon/verify/",
			want: true,
		},
		{
			name: "child_contains_parent",
			a:    "internal/daemon/verify/",
			b:    "internal/daemon/",
			want: true,
		},
		{
			name: "unrelated_paths",
			a:    "internal/model/",
			b:    "internal/daemon/",
			want: false,
		},
		{
			name: "shared_prefix_not_at_boundary",
			a:    "internal/da",
			b:    "internal/daemon/",
			want: false,
		},
		{
			name: "file_in_directory",
			a:    "internal/daemon/",
			b:    "internal/daemon/handler.go",
			want: true,
		},
		{
			name: "file_exact_match",
			a:    "cmd/main.go",
			b:    "cmd/main.go",
			want: true,
		},
		{
			name: "different_files_same_dir",
			a:    "cmd/main.go",
			b:    "cmd/util.go",
			want: false,
		},
		{
			name: "empty_a",
			a:    "",
			b:    "internal/daemon/",
			want: false,
		},
		{
			name: "empty_b",
			a:    "internal/daemon/",
			b:    "",
			want: false,
		},
		{
			name: "both_empty",
			a:    "",
			b:    "",
			want: false,
		},
		{
			name: "deep_nesting_overlap",
			a:    "internal/",
			b:    "internal/daemon/queue/handler.go",
			want: true,
		},
		{
			name: "sibling_directories",
			a:    "internal/daemon/queue/",
			b:    "internal/daemon/result/",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := HasPathOverlap(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("HasPathOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestHasTaskPathOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		paths1 []string
		paths2 []string
		want   bool
	}{
		{
			name:   "overlap_in_one_pair",
			paths1: []string{"internal/model/", "cmd/main.go"},
			paths2: []string{"internal/model/queue.go"},
			want:   true,
		},
		{
			name:   "no_overlap",
			paths1: []string{"internal/model/"},
			paths2: []string{"internal/daemon/"},
			want:   false,
		},
		{
			name:   "empty_paths1",
			paths1: nil,
			paths2: []string{"internal/daemon/"},
			want:   false,
		},
		{
			name:   "empty_paths2",
			paths1: []string{"internal/daemon/"},
			paths2: nil,
			want:   false,
		},
		{
			name:   "both_empty",
			paths1: nil,
			paths2: nil,
			want:   false,
		},
		{
			name:   "multiple_paths_one_overlap",
			paths1: []string{"pkg/util/", "internal/daemon/"},
			paths2: []string{"cmd/main.go", "internal/daemon/handler.go"},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := HasTaskPathOverlap(tt.paths1, tt.paths2)
			if got != tt.want {
				t.Errorf("HasTaskPathOverlap() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindOverlappingTask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		candidate    *model.Task
		inFlight     []inFlightPathEntry
		wantTaskID   string
		wantConflict bool
	}{
		{
			name: "no_overlap",
			candidate: &model.Task{
				ID:            "task-candidate",
				ExpectedPaths: []string{"internal/model/"},
			},
			inFlight: []inFlightPathEntry{
				{TaskID: "task-1", ExpectedPaths: []string{"internal/daemon/"}},
			},
			wantTaskID:   "",
			wantConflict: false,
		},
		{
			name: "overlap_found",
			candidate: &model.Task{
				ID:            "task-candidate",
				ExpectedPaths: []string{"internal/daemon/handler.go"},
			},
			inFlight: []inFlightPathEntry{
				{TaskID: "task-1", ExpectedPaths: []string{"internal/daemon/"}},
			},
			wantTaskID:   "task-1",
			wantConflict: true,
		},
		{
			// STRICT path: candidate with empty expected_paths is conservatively
			// blocked when any in-flight task exists (defensive guard).
			name: "candidate_no_paths_blocked_by_in_flight",
			candidate: &model.Task{
				ID:            "task-candidate",
				ExpectedPaths: nil,
			},
			inFlight: []inFlightPathEntry{
				{TaskID: "task-1", ExpectedPaths: []string{"internal/daemon/"}},
			},
			wantTaskID:   "task-1",
			wantConflict: true,
		},
		{
			// Candidate with empty paths but no in-flight: nothing to overlap.
			name: "candidate_no_paths_no_in_flight",
			candidate: &model.Task{
				ID:            "task-candidate",
				ExpectedPaths: nil,
			},
			inFlight:     nil,
			wantTaskID:   "",
			wantConflict: false,
		},
		{
			name: "empty_in_flight",
			candidate: &model.Task{
				ID:            "task-candidate",
				ExpectedPaths: []string{"internal/daemon/"},
			},
			inFlight:     nil,
			wantTaskID:   "",
			wantConflict: false,
		},
		{
			name: "multiple_in_flight_second_overlaps",
			candidate: &model.Task{
				ID:            "task-candidate",
				ExpectedPaths: []string{"pkg/util/helper.go"},
			},
			inFlight: []inFlightPathEntry{
				{TaskID: "task-1", ExpectedPaths: []string{"internal/daemon/"}},
				{TaskID: "task-2", ExpectedPaths: []string{"pkg/util/"}},
			},
			wantTaskID:   "task-2",
			wantConflict: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			taskID, _, _ := findOverlappingTask(tt.candidate, tt.inFlight)
			gotConflict := taskID != ""
			if gotConflict != tt.wantConflict {
				t.Errorf("findOverlappingTask() conflict = %v, want %v", gotConflict, tt.wantConflict)
			}
			if taskID != tt.wantTaskID {
				t.Errorf("findOverlappingTask() taskID = %q, want %q", taskID, tt.wantTaskID)
			}
		})
	}
}

func TestCollectInFlightPaths(t *testing.T) {
	t.Parallel()
	neverExpired := func(_ *string) bool { return false }
	alwaysExpired := func(_ *string) bool { return true }
	expires := "2099-01-01T00:00:00Z"

	queues := map[string]*taskQueueEntry{
		"worker1.yaml": {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "task-1",
						Status:         model.StatusInProgress,
						LeaseExpiresAt: &expires,
						ExpectedPaths:  []string{"internal/daemon/"},
					},
					{
						ID:             "task-2",
						Status:         model.StatusPending,
						LeaseExpiresAt: nil,
						ExpectedPaths:  []string{"internal/model/"},
					},
				},
			},
		},
		"worker2.yaml": {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "task-3",
						Status:         model.StatusInProgress,
						LeaseExpiresAt: &expires,
						ExpectedPaths:  nil,
					},
					{
						ID:             "task-4",
						Status:         model.StatusInProgress,
						LeaseExpiresAt: &expires,
						ExpectedPaths:  []string{"pkg/util/"},
					},
				},
			},
		},
	}

	t.Run("collects_in_progress_including_empty_paths_as_sentinel", func(t *testing.T) {
		t.Parallel()
		entries := collectInFlightPaths(queues, neverExpired)
		// STRICT: in-flight tasks with empty expected_paths are recorded with
		// SentinelUnknownPaths so they conflict with everything (defensive
		// guard for legacy / corrupted state — validation should reject empty
		// expected_paths).
		// Expected: task-1 (concrete path), task-3 (sentinel), task-4 (concrete).
		// task-2 is still skipped because it is pending, not in_progress.
		if len(entries) != 3 {
			t.Fatalf("got %d entries, want 3", len(entries))
		}
		ids := map[string]bool{}
		paths := map[string][]string{}
		for _, e := range entries {
			ids[e.TaskID] = true
			paths[e.TaskID] = e.ExpectedPaths
		}
		for _, id := range []string{"task-1", "task-3", "task-4"} {
			if !ids[id] {
				t.Errorf("missing %s", id)
			}
		}
		if got := paths["task-3"]; len(got) != 1 || got[0] != SentinelUnknownPaths {
			t.Errorf("task-3 paths = %v, want [%q]", got, SentinelUnknownPaths)
		}
	})

	t.Run("skips_expired_leases", func(t *testing.T) {
		t.Parallel()
		entries := collectInFlightPaths(queues, alwaysExpired)
		if len(entries) != 0 {
			t.Fatalf("got %d entries, want 0 (all expired)", len(entries))
		}
	})
}

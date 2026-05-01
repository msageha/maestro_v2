package daemon

import (
	"sort"
	"sync"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// statusRecorder substitutes for the real tmux setter during tests.
// Returns a deterministic snapshot of (agentID, status) pairs in call order.
type statusRecorder struct {
	mu    sync.Mutex
	calls []struct{ agentID, status string }
}

func (r *statusRecorder) record(agentID, status string, _ *QueueHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct{ agentID, status string }{agentID, status})
}

// statusByAgent returns the latest recorded status for each agent (or "" when
// no call was made for that agent).
func (r *statusRecorder) statusByAgent() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.calls))
	for _, c := range r.calls {
		out[c.agentID] = c.status
	}
	return out
}

// agentsCalled returns the unique agent IDs that received a sync call,
// sorted for stable assertion order.
func (r *statusRecorder) agentsCalled() []string {
	m := r.statusByAgent()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// installStatusRecorder swaps agentStatusSetter for the duration of a test
// and restores the original on cleanup.
func installStatusRecorder(t *testing.T) *statusRecorder {
	t.Helper()
	rec := &statusRecorder{}
	prev := agentStatusSetter
	agentStatusSetter = rec.record
	t.Cleanup(func() { agentStatusSetter = prev })
	return rec
}

func TestHasInProgressTasks(t *testing.T) {
	tests := []struct {
		name  string
		tasks []model.Task
		want  bool
	}{
		{"nil", nil, false},
		{"empty", []model.Task{}, false},
		{"pending only", []model.Task{{Status: model.StatusPending}}, false},
		{"completed only", []model.Task{{Status: model.StatusCompleted}}, false},
		{"in_progress", []model.Task{{Status: model.StatusInProgress}}, true},
		{"mixed with in_progress", []model.Task{
			{Status: model.StatusCompleted},
			{Status: model.StatusInProgress},
		}, true},
		{"mixed without in_progress", []model.Task{
			{Status: model.StatusPending},
			{Status: model.StatusCompleted},
			{Status: model.StatusFailed},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInProgressTasks(tt.tasks); got != tt.want {
				t.Errorf("hasInProgressTasks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasInProgressCommands(t *testing.T) {
	tests := []struct {
		name     string
		commands []model.Command
		want     bool
	}{
		{"nil", nil, false},
		{"empty", []model.Command{}, false},
		{"pending only", []model.Command{{Status: model.StatusPending}}, false},
		{"in_progress", []model.Command{{Status: model.StatusInProgress}}, true},
		{"completed", []model.Command{{Status: model.StatusCompleted}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInProgressCommands(tt.commands); got != tt.want {
				t.Errorf("hasInProgressCommands() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSyncAllConfiguredWorkers_ResetsBusyOnQueueLessWorkers pins the
// 2026-04-29 e2e regression: worker4 stayed at @status=busy while no
// task queue file existed, no command was active, no dispatch was in
// flight, and yet `maestro status --json` reported the worker as busy.
// The prior implementation iterated only the queue files present in
// s.tasks, so a worker without a queue file was never visited and a
// stale @status=busy from a previous session never got cleared. The
// fix enumerates every configured worker and forces idle on those
// without an in-progress task.
func TestSyncAllConfiguredWorkers_ResetsBusyOnQueueLessWorkers(t *testing.T) {
	rec := installStatusRecorder(t)
	qh := &QueueHandler{}
	qh.config.Agents.Workers.Count = 4

	// Only worker1 has a queue file (in-progress) — workers 2/3/4 have
	// no queue at all.  All three of those must be driven to idle.
	tasks := map[string]*taskQueueEntry{
		"/some/path/worker1.yaml": {
			Queue: model.TaskQueue{Tasks: []model.Task{{Status: model.StatusInProgress}}},
		},
	}

	syncAllConfiguredWorkers(qh, tasks)

	got := rec.statusByAgent()
	want := map[string]string{
		"worker1": "busy",
		"worker2": "idle",
		"worker3": "idle",
		"worker4": "idle",
	}
	for agent, expected := range want {
		if got[agent] != expected {
			t.Errorf("agent %s: got %q, want %q (full=%v)", agent, got[agent], expected, got)
		}
	}
}

// TestSyncAllConfiguredWorkers_StrayQueueFileOutsideConfigStillSynced
// guards the operator-extension scenario: an operator with a manually
// added queue file outside the configured count (e.g. count=2 but a
// worker3.yaml exists from a prior larger formation) should still see
// its @status reflect reality. The configured workers always sync;
// extras get covered by a final pass.
func TestSyncAllConfiguredWorkers_StrayQueueFileOutsideConfigStillSynced(t *testing.T) {
	rec := installStatusRecorder(t)
	qh := &QueueHandler{}
	qh.config.Agents.Workers.Count = 2

	tasks := map[string]*taskQueueEntry{
		"/some/path/worker1.yaml": {
			Queue: model.TaskQueue{Tasks: []model.Task{{Status: model.StatusInProgress}}},
		},
		"/some/path/worker3.yaml": {
			Queue: model.TaskQueue{Tasks: []model.Task{{Status: model.StatusCompleted}}},
		},
	}

	syncAllConfiguredWorkers(qh, tasks)

	got := rec.statusByAgent()
	want := map[string]string{
		"worker1": "busy",
		"worker2": "idle",
		"worker3": "idle",
	}
	for agent, expected := range want {
		if got[agent] != expected {
			t.Errorf("agent %s: got %q, want %q (full=%v)", agent, got[agent], expected, got)
		}
	}
}

// TestSyncAllConfiguredWorkers_ZeroCountFallsBackToQueueFiles guards
// against a regression in the count<=0 fallback path. With no
// configured workers, the routine still must sync whatever queue files
// happen to exist so prior dashboard behaviour for ad-hoc test setups
// is preserved.
func TestSyncAllConfiguredWorkers_ZeroCountFallsBackToQueueFiles(t *testing.T) {
	rec := installStatusRecorder(t)
	qh := &QueueHandler{}
	qh.config.Agents.Workers.Count = 0

	tasks := map[string]*taskQueueEntry{
		"/some/path/worker1.yaml": {
			Queue: model.TaskQueue{Tasks: []model.Task{{Status: model.StatusInProgress}}},
		},
	}

	syncAllConfiguredWorkers(qh, tasks)

	got := rec.statusByAgent()
	if got["worker1"] != "busy" {
		t.Errorf("worker1 status=%q, want busy (full=%v)", got["worker1"], got)
	}
	// Workers 2/3/4 are not in the queue file map AND not enumerated
	// (count=0), so they receive no calls.
	for _, unwanted := range []string{"worker2", "worker3", "worker4"} {
		if _, called := got[unwanted]; called {
			t.Errorf("agent %s should not have been synced when count=0 and queue file absent (full=%v)",
				unwanted, got)
		}
	}
}

func TestHasInProgressNotifications(t *testing.T) {
	tests := []struct {
		name          string
		notifications []model.Notification
		want          bool
	}{
		{"nil", nil, false},
		{"empty", []model.Notification{}, false},
		{"pending only", []model.Notification{{Status: model.StatusPending}}, false},
		{"completed only", []model.Notification{{Status: model.StatusCompleted}}, false},
		{"in_progress", []model.Notification{{Status: model.StatusInProgress}}, true},
		{"completed after dispatch", []model.Notification{
			{Status: model.StatusCompleted},
			{Status: model.StatusPending},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInProgressNotifications(tt.notifications); got != tt.want {
				t.Errorf("hasInProgressNotifications() = %v, want %v", got, tt.want)
			}
		})
	}
}

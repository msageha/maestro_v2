package plan

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// PR #56 review finding #5: `plan add-retry-task` (primary) must inherit the
// original task's resume_hint — dropping it silently reverted an explicit
// `resume_hint: deny` on a non-idempotent task to the default-allow policy.
func TestBuildPrimaryRetryTask_InheritsResumeHint(t *testing.T) {
	t.Parallel()
	origTaskCache := map[string]model.Task{
		"task_orig": {ID: "task_orig", ResumeHint: model.ResumeHintDeny},
	}
	opts := inheritRetryFieldsFromOriginal(RetryOptions{RetryOf: "task_orig"}, origTaskCache)
	if opts.ResumeHint != model.ResumeHintDeny {
		t.Fatalf("inherited ResumeHint = %q, want %q", opts.ResumeHint, model.ResumeHintDeny)
	}
	task := buildPrimaryRetryTask(opts, "task_retry", nil, "worker1")
	if task.resumeHint != model.ResumeHintDeny {
		t.Errorf("primary retryQueueTask.resumeHint = %q, want %q", task.resumeHint, model.ResumeHintDeny)
	}
}

// Cascade-recovered replacements must inherit resume_hint from their own
// original task as well.
func TestBuildCascadeQueueTask_InheritsResumeHint(t *testing.T) {
	t.Parallel()
	origTaskCache := map[string]model.Task{
		"task_cascade_orig": {
			ID:         "task_cascade_orig",
			Purpose:    "p",
			Content:    "c",
			ResumeHint: model.ResumeHintAllow,
		},
	}
	state := &model.CommandState{TaskTracking: model.TaskTracking{TaskDependencies: map[string][]string{}}}
	cr := CascadeRecoveredTask{TaskID: "task_cascade_new", Worker: "worker2", Replaced: "task_cascade_orig"}

	task := buildCascadeQueueTask(cr, RetryOptions{CommandID: "cmd_x"}, state, origTaskCache)
	if task.resumeHint != model.ResumeHintAllow {
		t.Errorf("cascade retryQueueTask.resumeHint = %q, want %q", task.resumeHint, model.ResumeHintAllow)
	}
}

// writeRetryQueueEntry must persist the hint into the queue YAML so the
// daemon's ResumeEligible sees it on the replacement task.
func TestWriteRetryQueueEntry_PersistsResumeHint(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}

	task := retryQueueTask{
		taskID:     "task_hint_rt",
		commandID:  "cmd_hint",
		purpose:    "p",
		content:    "c",
		workerID:   "worker1",
		resumeHint: model.ResumeHintDeny,
	}
	if err := writeRetryQueueEntry(maestroDir, task, "2026-07-24T01:00:00Z", nil); err != nil {
		t.Fatalf("writeRetryQueueEntry: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if len(tq.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tq.Tasks))
	}
	if tq.Tasks[0].ResumeHint != model.ResumeHintDeny {
		t.Errorf("persisted ResumeHint = %q, want %q", tq.Tasks[0].ResumeHint, model.ResumeHintDeny)
	}
}

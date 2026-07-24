package envelope

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// Issue #55 acceptance (d): the resume nudge must carry the NEW lease epoch
// in both the header and the result-write protocol line so the worker's
// eventual `maestro result write` passes fencing instead of hitting
// FENCING_REJECT with the interrupted epoch.
func TestBuildWorkerResumeEnvelope_CarriesNewEpoch(t *testing.T) {
	t.Parallel()
	task := model.Task{
		ID:        "task_1784809943_08dc4796d114d336",
		CommandID: "cmd_1784809943_aaaaaaaaaaaaaaaa",
		Content:   "SHOULD-NOT-APPEAR: full task body",
	}
	env := BuildWorkerResumeEnvelope(task, "worker4", 4, 2)

	if !strings.Contains(env, "[maestro] resume task_id:task_1784809943_08dc4796d114d336") {
		t.Errorf("missing resume header with task id:\n%s", env)
	}
	if !strings.Contains(env, "lease_epoch:4") {
		t.Errorf("header must carry the new lease epoch:\n%s", env)
	}
	if !strings.Contains(env, "maestro result write worker4 --task-id task_1784809943_08dc4796d114d336 --command-id cmd_1784809943_aaaaaaaaaaaaaaaa --lease-epoch 4") {
		t.Errorf("result-write line must carry the new lease epoch:\n%s", env)
	}
	if strings.Contains(env, "SHOULD-NOT-APPEAR") {
		t.Errorf("resume nudge must not re-deliver the task body (the pane already holds it):\n%s", env)
	}
}

// PR #56 review finding #10: the CLI requires --exit-code when
// --status=failed (cmd_result.go), so both worker envelopes must tell the
// worker about it — otherwise a failure report bounces on validation.
func TestWorkerEnvelopes_MentionExitCodeForFailedStatus(t *testing.T) {
	t.Parallel()
	task := model.Task{
		ID:        "task_1784809943_08dc4796d114d336",
		CommandID: "cmd_1784809943_aaaaaaaaaaaaaaaa",
	}
	full := BuildWorkerEnvelope(task, NewRawContent("body").Sanitize(), "worker1", 2, 1)
	if !strings.Contains(full, "--exit-code") {
		t.Errorf("full envelope must mention --exit-code for failed status:\n%s", full)
	}
	nudge := BuildWorkerResumeEnvelope(task, "worker1", 3, 1)
	if !strings.Contains(nudge, "--exit-code") {
		t.Errorf("resume envelope must mention --exit-code for failed status:\n%s", nudge)
	}
}

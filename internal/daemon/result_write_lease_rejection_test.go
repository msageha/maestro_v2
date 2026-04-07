package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// bumpQueueLeaseEpoch rewrites the worker's queue file so the named task's
// lease_epoch is incremented (and the task is forced back to in_progress so
// the best-effort check sees a live but mismatched lease).
func bumpQueueLeaseEpoch(t *testing.T, d *Daemon, workerID, taskID string, newEpoch int) {
	t.Helper()
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}
	owner := workerID
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == taskID {
			tq.Tasks[i].LeaseEpoch = newEpoch
			tq.Tasks[i].Status = model.StatusInProgress
			tq.Tasks[i].LeaseOwner = &owner
		}
	}
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("rewrite worker queue: %v", err)
	}
}

func readResultsFile(t *testing.T, d *Daemon, workerID string) model.TaskResultFile {
	t.Helper()
	path := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("parse result file: %v", err)
	}
	return rf
}

// TestResultWrite_LeaseRevoke_PersistsRejection covers H4: a stale worker
// re-sends a result_write with new learnings/skill_candidates after its
// lease has been revoked. The core result is idempotent (already
// committed), but the previously-silent best-effort drop must now
// (a) be persisted as a RejectedSubmission audit record and
// (b) surface a lease_rejection_id in the success response.
func TestResultWrite_LeaseRevoke_PersistsRejection(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// 1. Initial successful result_write (no learnings yet — clean baseline).
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("initial write: expected success, got error: %v", resp.Error)
	}

	// 2. Simulate lease revoke: bump lease_epoch on the queue side and
	// reset the task back to in_progress with a new owner.
	bumpQueueLeaseEpoch(t, d, workerID, taskID, leaseEpoch+1)

	// 3. Stale worker re-sends with the OLD lease_epoch and now attaches
	// learnings + skill_candidates. phaseA returns an idempotent
	// success (existing terminal result for the same task_id is found in
	// the results file before the queue lease check), and the
	// best-effort lease check then detects the mismatch.
	staleParams := ResultWriteParams{
		Reporter:        workerID,
		TaskID:          taskID,
		CommandID:       commandID,
		LeaseEpoch:      leaseEpoch, // stale
		Status:          "completed",
		Summary:         "done",
		Learnings:       []string{"stale learning A", "stale learning B"},
		SkillCandidates: []string{"stale skill"},
	}
	resp2 := d.api.handleResultWrite(makeResultWriteRequest(t, staleParams))
	if !resp2.Success {
		t.Fatalf("stale write: expected idempotent success, got error: %v", resp2.Error)
	}

	var payload map[string]string
	if err := json.Unmarshal(resp2.Data, &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload["lease_rejection_id"] == "" {
		t.Fatalf("expected lease_rejection_id in response, got payload=%v", payload)
	}
	if payload["lease_rejection_warning"] == "" {
		t.Errorf("expected lease_rejection_warning in response, got payload=%v", payload)
	}
	rejectionID := payload["lease_rejection_id"]

	// 4. Learnings file must be empty (best-effort writes were skipped).
	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	if _, err := os.Stat(learningsPath); !os.IsNotExist(err) {
		t.Errorf("learnings file should not exist after lease revoke, stat err=%v", err)
	}

	// 5. RejectedSubmissions must contain the dropped payload.
	rf := readResultsFile(t, d, workerID)
	if len(rf.RejectedSubmissions) != 1 {
		t.Fatalf("expected 1 rejected submission, got %d", len(rf.RejectedSubmissions))
	}
	rs := rf.RejectedSubmissions[0]
	if rs.ID != rejectionID {
		t.Errorf("rejection id mismatch: file=%q response=%q", rs.ID, rejectionID)
	}
	if rs.TaskID != taskID || rs.CommandID != commandID || rs.Reporter != workerID {
		t.Errorf("rejection identity mismatch: %+v", rs)
	}
	if rs.RequestLeaseEpoch != leaseEpoch || rs.QueueLeaseEpoch != leaseEpoch+1 {
		t.Errorf("rejection epochs: req=%d queue=%d, want %d/%d",
			rs.RequestLeaseEpoch, rs.QueueLeaseEpoch, leaseEpoch, leaseEpoch+1)
	}
	if len(rs.LostLearnings) != 2 || rs.LostLearnings[0] != "stale learning A" {
		t.Errorf("lost learnings mismatch: %v", rs.LostLearnings)
	}
	if len(rs.LostSkillCandidates) != 1 || rs.LostSkillCandidates[0] != "stale skill" {
		t.Errorf("lost skill candidates mismatch: %v", rs.LostSkillCandidates)
	}
	if rs.Reason == "" {
		t.Errorf("rejection reason must be populated")
	}
	if rs.DedupKey == "" {
		t.Errorf("dedup key must be populated")
	}

	// 6. Idempotent stale retry with identical payload must NOT grow the
	// rejection log — it returns the same rejection_id.
	resp3 := d.api.handleResultWrite(makeResultWriteRequest(t, staleParams))
	if !resp3.Success {
		t.Fatalf("stale retry: expected success, got error: %v", resp3.Error)
	}
	var payload3 map[string]string
	json.Unmarshal(resp3.Data, &payload3)
	if payload3["lease_rejection_id"] != rejectionID {
		t.Errorf("dedup retry should reuse rejection_id: got %q, want %q",
			payload3["lease_rejection_id"], rejectionID)
	}
	rf2 := readResultsFile(t, d, workerID)
	if len(rf2.RejectedSubmissions) != 1 {
		t.Errorf("dedup retry must not duplicate rejection record, got %d entries",
			len(rf2.RejectedSubmissions))
	}

	// 7. A second stale retry with DIFFERENT learnings content produces a
	// new rejection record (different DedupKey).
	differentParams := staleParams
	differentParams.Learnings = []string{"different learning"}
	resp4 := d.api.handleResultWrite(makeResultWriteRequest(t, differentParams))
	if !resp4.Success {
		t.Fatalf("stale retry (different): expected success, got error: %v", resp4.Error)
	}
	rf3 := readResultsFile(t, d, workerID)
	if len(rf3.RejectedSubmissions) != 2 {
		t.Errorf("expected 2 rejection records after different payload, got %d",
			len(rf3.RejectedSubmissions))
	}
}

// TestResultWrite_LeaseRevoke_PartialAndRetrySafePropagation verifies that
// the originally committed TaskResult's PartialChangesPossible / RetrySafe
// flags are NOT mutated when a stale worker re-sends the same task with
// different flag values after its lease has been revoked. The idempotent
// path must return the existing result unchanged.
func TestResultWrite_LeaseRevoke_PartialAndRetrySafePropagation(t *testing.T) {
	d := newTestDaemonWithLearnings(t)
	taskID := "task_0000000002_abcdef02"
	commandID := "cmd_0000000002_abcdef02"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// 1. Initial successful write with partial_changes_possible=true and
	// retry_safe=false (the conservative "do not retry" combination).
	resp := d.api.handleResultWrite(makeResultWriteRequest(t, ResultWriteParams{
		Reporter:               workerID,
		TaskID:                 taskID,
		CommandID:              commandID,
		LeaseEpoch:             leaseEpoch,
		Status:                 "completed",
		Summary:                "done",
		PartialChangesPossible: true,
		RetrySafe:              false,
	}))
	if !resp.Success {
		t.Fatalf("initial write: expected success, got error: %v", resp.Error)
	}

	rfBefore := readResultsFile(t, d, workerID)
	if len(rfBefore.Results) != 1 {
		t.Fatalf("expected 1 result entry after initial write, got %d", len(rfBefore.Results))
	}
	if !rfBefore.Results[0].PartialChangesPossible {
		t.Fatalf("initial write must persist PartialChangesPossible=true")
	}
	if rfBefore.Results[0].RetrySafe {
		t.Fatalf("initial write must persist RetrySafe=false")
	}

	// 2. Revoke the lease.
	bumpQueueLeaseEpoch(t, d, workerID, taskID, leaseEpoch+1)

	// 3. Stale worker resubmits with FLIPPED flag values.
	resp2 := d.api.handleResultWrite(makeResultWriteRequest(t, ResultWriteParams{
		Reporter:               workerID,
		TaskID:                 taskID,
		CommandID:              commandID,
		LeaseEpoch:             leaseEpoch, // stale
		Status:                 "completed",
		Summary:                "done",
		PartialChangesPossible: false, // attempts to flip
		RetrySafe:              true,  // attempts to flip
	}))
	if !resp2.Success {
		t.Fatalf("stale write: expected idempotent success, got error: %v", resp2.Error)
	}

	// 4. The committed TaskResult flags must be unchanged.
	rfAfter := readResultsFile(t, d, workerID)
	if len(rfAfter.Results) != 1 {
		t.Fatalf("idempotent stale resubmit must not append a second result, got %d", len(rfAfter.Results))
	}
	if !rfAfter.Results[0].PartialChangesPossible {
		t.Errorf("PartialChangesPossible must remain true after stale flip, got false")
	}
	if rfAfter.Results[0].RetrySafe {
		t.Errorf("RetrySafe must remain false after stale flip, got true")
	}
}

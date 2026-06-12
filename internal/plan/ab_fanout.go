package plan

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// A/B candidate fan-out (docs/design/ab_candidate_selection.md §3-4).
//
// Runs AFTER a successful plan submit, outside the submit pipeline's locks,
// as a strictly additive pass: any failure here is reported as a warning and
// the canonical task continues on the normal single-candidate pipeline. The
// shadow candidate is a copy of the canonical queue row assigned to a worker
// of a DIFFERENT runtime; both rows carry ab_group_id, and the CandidateGroup
// in command state is the SSOT for the race.

// maybeCreateABCandidates fans out eligible just-submitted tasks into A/B
// candidate groups. Returns advisory warnings (never errors): A/B is
// additive and must not fail the submit that already committed.
//
// Locking (per task, matching the canonical queue→state order): the two
// affected worker queues are locked via lockQueueKeys, then the command
// state lock is taken for the group registration.
func maybeCreateABCandidates(opts SubmitOptions, sm *StateManager, res *SubmitResult, pinned map[string]bool) []string {
	if res == nil || len(res.Tasks) == 0 || opts.DryRun {
		return nil
	}
	cfg := opts.Config
	if !cfg.ABTest.EffectiveEnabled() {
		return nil
	}
	if !cfg.Worktree.Enabled {
		// Candidate isolation requires worktree mode.
		return []string{"ab_test enabled but worktree mode is off; A/B fan-out skipped"}
	}

	workerStates, err := BuildWorkerStates(opts.MaestroDir, cfg.Agents.Workers)
	if err != nil {
		return []string{fmt.Sprintf("ab fan-out skipped: build worker states: %v", err)}
	}

	var warnings []string
	// Selection-quality precondition (advisory; the race still runs): the
	// Stage 0 / candidate-suite signal reads the command-scoped verify
	// snapshot. Without one it degenerates to the weak default
	// (`git diff --check`) and the race resolves as a first-finisher tie
	// (2026-06-13 E2E finding F-1).
	if _, err := os.Stat(filepath.Join(opts.MaestroDir, "state", "verify", res.CommandID+".yaml")); err != nil {
		warnings = append(warnings,
			"ab_test: no verify snapshot for this command — candidate selection falls back to the weak default signal; write build/test commands via `maestro verify write` before plan submit")
	}

	minBloom := cfg.ABTest.EffectiveMinBloomLevel()
	for _, tr := range res.Tasks {
		if pinned[tr.Name] {
			continue // an explicit worker pin expresses operator intent; no shadow
		}
		shadowWorker, shadowModel, ok := pickShadowWorker(workerStates, tr.Worker, tr.Model, cfg.Limits.MaxPendingTasksPerWorker)
		if !ok {
			warnings = append(warnings,
				fmt.Sprintf("ab fan-out skipped for %s: no available worker of a different runtime", tr.TaskID))
			continue
		}
		if w := createABCandidate(opts, sm, tr, shadowWorker, shadowModel, minBloom); w != "" {
			warnings = append(warnings, w)
		} else {
			// Reflect the shadow assignment in the local snapshot so a
			// multi-task submit spreads shadows across workers.
			for i := range workerStates {
				if workerStates[i].WorkerID == shadowWorker {
					workerStates[i].PendingCount++
				}
			}
		}
	}
	return warnings
}

// pickShadowWorker selects the least-loaded worker whose model resolves to a
// DIFFERENT runtime than the canonical assignment. Returns ok=false when no
// such worker exists or all are at capacity.
func pickShadowWorker(workerStates []WorkerState, canonicalWorker, canonicalModel string, maxPending int) (workerID, workerModel string, ok bool) {
	canonicalRuntime, _ := model.ParseRuntimeFromModel(canonicalModel)
	if maxPending <= 0 {
		maxPending = 10 // mirror AssignWorkers' default
	}
	var best *WorkerState
	for i := range workerStates {
		ws := &workerStates[i]
		if ws.WorkerID == canonicalWorker {
			continue
		}
		rt, _ := model.ParseRuntimeFromModel(ws.Model)
		if rt == canonicalRuntime {
			continue
		}
		if ws.PendingCount >= maxPending {
			continue
		}
		if best == nil || ws.PendingCount < best.PendingCount ||
			(ws.PendingCount == best.PendingCount && ws.WorkerID < best.WorkerID) {
			best = ws
		}
	}
	if best == nil {
		return "", "", false
	}
	return best.WorkerID, best.Model, true
}

// createABCandidate atomically (queue locks → state lock) creates the shadow
// queue row, tags the canonical row with the group ID, and registers the
// CandidateGroup + shadow task in command state. Returns a warning string on
// any failure ("" on success).
//
// Write order is STATE FIRST, then canonical tag, then shadow row: every
// crash artifact is then visible from the durable CandidateGroups map and
// repairable by the state-driven A/B recovery (a group whose rows are
// untagged/missing degrades safely), whereas an orphan queue tag without a
// group would route work to a candidate worktree nobody ever intakes.
// Non-crash write failures are rolled back from byte snapshots; a failed
// rollback leaves only state-first artifacts, which the same recovery
// handles.
func createABCandidate(opts SubmitOptions, sm *StateManager, tr SubmitTaskResult, shadowWorker, shadowModel string, minBloom int) string {
	canonicalQueue := workerQueuePath(opts.MaestroDir, tr.Worker)
	shadowQueue := workerQueuePath(opts.MaestroDir, shadowWorker)

	unlock := lockQueueKeys(opts.LockMap, []string{"queue:" + tr.Worker, "queue:" + shadowWorker})
	defer unlock()
	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	// --- Load + eligibility (everything re-checked under the locks; all
	// reads happen before the first write so read failures abort cleanly) ---
	canonBytes, err := os.ReadFile(canonicalQueue) //nolint:gosec // controlled queue dir
	if err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: read canonical queue: %v", tr.TaskID, err)
	}
	var canonTQ model.TaskQueue
	if err := yamlv3.Unmarshal(canonBytes, &canonTQ); err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: parse canonical queue: %v", tr.TaskID, err)
	}
	var canon *model.Task
	for i := range canonTQ.Tasks {
		if canonTQ.Tasks[i].ID == tr.TaskID {
			canon = &canonTQ.Tasks[i]
			break
		}
	}
	switch {
	case canon == nil:
		return fmt.Sprintf("ab fan-out skipped for %s: canonical row not found", tr.TaskID)
	case canon.Status != model.StatusPending:
		return "" // already dispatched (raced with a scan) — silently skip
	case canon.ABGroupID != "":
		return "" // idempotent: already fanned out
	case canon.BloomLevel < minBloom:
		return ""
	case canon.RunOnMain || canon.RunOnIntegration:
		return "" // verification / conflict-resolution tasks are out of scope
	}

	shadowBytes, err := os.ReadFile(shadowQueue) //nolint:gosec // controlled queue dir
	if err != nil && !os.IsNotExist(err) {
		return fmt.Sprintf("ab fan-out skipped for %s: read shadow queue: %v", tr.TaskID, err)
	}
	var shadowTQ model.TaskQueue
	if len(shadowBytes) > 0 {
		if err := yamlv3.Unmarshal(shadowBytes, &shadowTQ); err != nil {
			return fmt.Sprintf("ab fan-out skipped for %s: parse shadow queue: %v", tr.TaskID, err)
		}
	} else {
		shadowTQ = model.TaskQueue{SchemaVersion: 1, FileType: "queue_task"}
	}

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: load state: %v", tr.TaskID, err)
	}
	canonState, ok := state.TaskStates[tr.TaskID]
	if !ok {
		return fmt.Sprintf("ab fan-out skipped for %s: canonical task missing from state", tr.TaskID)
	}
	if _, g := state.FindCandidateGroupByTask(tr.TaskID); g != nil {
		// Idempotent at the state level too: a crash between the state and
		// queue writes leaves the group registered; completing the queue
		// side here would race the recovery that may already be degrading
		// it, so leave stragglers to R-AB.
		return ""
	}
	statePath, err := sm.StatePath(opts.CommandID)
	if err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: state path: %v", tr.TaskID, err)
	}
	stateBytes, err := os.ReadFile(statePath) //nolint:gosec // controlled state dir
	if err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: snapshot state: %v", tr.TaskID, err)
	}

	shadowID, err := model.NewTaskID(model.TaskIDCallerABCandidate)
	if err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: mint shadow ID: %v", tr.TaskID, err)
	}
	groupID := "abg_" + tr.TaskID
	now := nowUTC()

	shadow := *canon // copy all task content verbatim
	shadow.ID = shadowID
	shadow.ABGroupID = groupID
	shadow.Status = model.StatusPending
	shadow.DispatchID = ""
	shadow.Attempts = 0
	shadow.ExecutionRetries = 0
	shadow.OriginalTaskID = ""
	shadow.NotBefore = nil
	shadow.LastError = nil
	shadow.DeadLetteredAt = nil
	shadow.DeadLetterReason = nil
	shadow.LeaseOwner = nil
	shadow.LeaseExpiresAt = nil
	shadow.LeaseEpoch = 0
	shadow.InProgressAt = nil
	shadow.CreatedAt = now
	shadow.UpdatedAt = now
	shadowTQ.Tasks = append(shadowTQ.Tasks, shadow)

	canon.ABGroupID = groupID
	canon.UpdatedAt = now

	// --- State registration (in memory) ---
	state.TaskStates[shadowID] = canonState
	if state.TaskDependencies == nil {
		state.TaskDependencies = map[string][]string{}
	}
	deps := make([]string, len(state.TaskDependencies[tr.TaskID]))
	copy(deps, state.TaskDependencies[tr.TaskID])
	state.TaskDependencies[shadowID] = deps
	if state.CandidateGroups == nil {
		state.CandidateGroups = map[string]*model.CandidateGroup{}
	}
	state.CandidateGroups[groupID] = &model.CandidateGroup{
		Status:          model.ABGroupRacing,
		CanonicalTaskID: tr.TaskID,
		Candidates: []model.ABCandidate{
			{
				TaskID:     tr.TaskID,
				WorkerID:   tr.Worker,
				Model:      tr.Model,
				BloomLevel: canon.BloomLevel,
				Branch:     model.ABCandidateBranch(opts.CommandID, tr.TaskID),
			},
			{
				TaskID:     shadowID,
				WorkerID:   shadowWorker,
				Model:      shadowModel,
				BloomLevel: canon.BloomLevel,
				Branch:     model.ABCandidateBranch(opts.CommandID, shadowID),
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	state.UpdatedAt = now

	// --- Writes: state → canonical tag → shadow row ---
	if err := sm.SaveState(state); err != nil {
		return fmt.Sprintf("ab fan-out skipped for %s: save state: %v", tr.TaskID, err)
	}
	rollbackState := func() {
		if err := yamlutil.AtomicWriteRaw(statePath, stateBytes); err != nil {
			// Leftover group without queue rows — degraded by R-AB recovery.
			slogc().Error("ab_fanout_rollback_state_failed", "task", tr.TaskID, "error", err)
		}
	}
	if err := yamlutil.AtomicWrite(canonicalQueue, canonTQ); err != nil {
		rollbackState()
		return fmt.Sprintf("ab fan-out skipped for %s: write canonical queue: %v", tr.TaskID, err)
	}
	if err := yamlutil.AtomicWrite(shadowQueue, shadowTQ); err != nil {
		if rerr := yamlutil.AtomicWriteRaw(canonicalQueue, canonBytes); rerr != nil {
			slogc().Error("ab_fanout_rollback_canonical_failed", "task", tr.TaskID, "error", rerr)
		}
		rollbackState()
		return fmt.Sprintf("ab fan-out skipped for %s: write shadow queue: %v", tr.TaskID, err)
	}

	slogc().Info("ab_candidate_created",
		"command", opts.CommandID, "group", groupID,
		"canonical_task", tr.TaskID, "canonical_worker", tr.Worker,
		"shadow_task", shadowID, "shadow_worker", shadowWorker, "shadow_model", shadowModel)
	return ""
}

// collectPinnedTaskNames gathers task names with an explicit worker pin from
// every submit input shape (flat tasks, phase task lists). Pinned tasks are
// never fanned out — the pin expresses operator intent about WHO runs it.
func collectPinnedTaskNames(input SubmitInput) map[string]bool {
	pinned := map[string]bool{}
	for _, t := range input.Tasks {
		if t.WorkerID != "" {
			pinned[t.Name] = true
		}
	}
	for _, ph := range input.Phases {
		for _, t := range ph.Tasks {
			if t.WorkerID != "" {
				pinned[t.Name] = true
			}
		}
	}
	return pinned
}

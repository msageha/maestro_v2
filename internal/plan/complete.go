package plan

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// CompleteOptions holds the configuration for completing a command.
type CompleteOptions struct {
	CommandID  string
	Summary    string
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
}

// CompleteResult contains the outcome of a command completion operation.
type CompleteResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
}

// completeIntent is the WAL record written before the multi-step completion
// sequence. If a crash occurs mid-sequence, the next Complete() call for the
// same command will discover the intent and replay all steps idempotently
// (CR-019). Note: intents for other commands are not recovered here; daemon
// startup should scan the intents directory for broader recovery.
type completeIntent struct {
	SchemaVersion int                       `yaml:"schema_version"`
	FileType      string                    `yaml:"file_type"`
	CommandID     string                    `yaml:"command_id"`
	Summary       string                    `yaml:"summary"`
	ResultStatus  model.Status              `yaml:"result_status"`
	PlanStatus    model.PlanStatus          `yaml:"plan_status"`
	TaskResults   []model.CommandResultTask `yaml:"task_results"`
	CreatedAt     string                    `yaml:"created_at"`
}

// Complete finalises a command by writing the result, updating the queue entry,
// and transitioning the state to a terminal plan_status.
//
// Lock ordering (canonical, consistent with the rest of the codebase):
//
//	queue:planner → state:{commandID} → result:planner
//
// All three locks are acquired here so the ordering is visible in one place.
// The helper functions (writeCommandResultLocked, updateCommandQueueEntryLocked)
// assume the caller already holds the required lock.
//
// Crash recovery (CR-019): an intent file is written before the 3-step
// sequence and removed after all steps succeed. On entry, any stale intent
// for this command is recovered by replaying the steps idempotently.
func Complete(opts CompleteOptions) (*CompleteResult, error) {
	if opts.LockMap == nil {
		return nil, fmt.Errorf("LockMap is required")
	}
	// Validate commandID early (before any path construction) to prevent
	// directory traversal via intent file paths.
	if filepath.Base(opts.CommandID) != opts.CommandID || opts.CommandID == "" || opts.CommandID == "." || opts.CommandID == ".." {
		return nil, fmt.Errorf("invalid command ID: %q", opts.CommandID)
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	// Acquire locks in canonical order: queue → state → result
	opts.LockMap.Lock("queue:planner")
	defer opts.LockMap.Unlock("queue:planner")

	// Schedule explicit removal of the per-command state lock entry after
	// the unlock runs. defer is LIFO, so this Remove fires last and drops
	// the now-idle entry from the MutexMap to prevent unbounded growth as
	// commands accumulate over a long-running daemon.
	defer opts.LockMap.Remove("state:" + opts.CommandID)
	sm.LockCommand(opts.CommandID) // state:{commandID}
	defer sm.UnlockCommand(opts.CommandID)

	// --- Intent recovery: replay stale intent if present (CR-019) ---
	intent, intentErr := readCompleteIntent(opts.MaestroDir, opts.CommandID)
	if intentErr != nil {
		// Corrupt/unreadable intent: log and quarantine by removing the broken
		// file so it doesn't block future calls. The normal flow will re-derive
		// the correct status from state.
		log.Printf("[WARN] Complete: corrupt intent file for command %s, removing: %v", opts.CommandID, intentErr)
		removeCompleteIntent(opts.MaestroDir, opts.CommandID)
	} else if intent != nil {
		// Validate intent matches the current command (defense against file corruption)
		if intent.CommandID != opts.CommandID {
			log.Printf("[WARN] Complete: intent command_id mismatch (intent=%s, opts=%s), removing corrupt intent", intent.CommandID, opts.CommandID)
			removeCompleteIntent(opts.MaestroDir, opts.CommandID)
		} else {
			log.Printf("[INFO] Complete: recovering stale intent for command %s", opts.CommandID)
			actualStatus, err := replayCompleteIntent(opts, sm, intent)
			if err != nil {
				return nil, fmt.Errorf("intent recovery: %w", err)
			}
			return &CompleteResult{
				CommandID: opts.CommandID,
				Status:    string(actualStatus),
			}, nil
		}
	}

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	// Idempotency: if already completed/failed/cancelled, return existing status
	if state.PlanStatus == model.PlanStatusCompleted || state.PlanStatus == model.PlanStatusFailed || state.PlanStatus == model.PlanStatusCancelled {
		return &CompleteResult{
			CommandID: opts.CommandID,
			Status:    string(state.PlanStatus),
		}, nil
	}

	// can-complete validation
	derivedPlanStatus, err := CanComplete(state)
	if err != nil {
		return nil, err
	}

	// Map PlanStatus to Status for result
	var resultStatus model.Status
	switch derivedPlanStatus {
	case model.PlanStatusCompleted:
		resultStatus = model.StatusCompleted
	case model.PlanStatusFailed:
		resultStatus = model.StatusFailed
	case model.PlanStatusCancelled:
		resultStatus = model.StatusCancelled
	default:
		return nil, fmt.Errorf("unexpected derived status: %s", derivedPlanStatus)
	}

	// Aggregate task results from results/worker{N}.yaml (lock-free: AtomicWrite
	// guarantees consistent snapshots; worst case is a slightly stale read).
	taskResults, partial, err := aggregateTaskResults(opts.MaestroDir, opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("aggregate results: %w", err)
	}
	if partial {
		log.Printf("[WARN] Complete: partial task results for command %s (some worker result files were unreadable)", opts.CommandID)
	}

	// --- Write intent before the multi-step sequence (CR-019) ---
	intent = &completeIntent{
		SchemaVersion: 1,
		FileType:      "intent_plan_complete",
		CommandID:     opts.CommandID,
		Summary:       opts.Summary,
		ResultStatus:  resultStatus,
		PlanStatus:    derivedPlanStatus,
		TaskResults:   taskResults,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeCompleteIntent(opts.MaestroDir, intent); err != nil {
		return nil, fmt.Errorf("write intent: %w", err)
	}

	// --- Execute the 3-step sequence (all steps are idempotent) ---
	if err := executeCompleteSteps(opts, sm, state, intent); err != nil {
		// Intent file is kept so the next call can recover.
		return nil, err
	}

	// --- Remove intent after all steps succeeded ---
	removeCompleteIntent(opts.MaestroDir, opts.CommandID)

	return &CompleteResult{
		CommandID: opts.CommandID,
		Status:    string(derivedPlanStatus),
	}, nil
}

// executeCompleteSteps runs the 3-step completion sequence. Each step is
// idempotent so it is safe to replay on recovery.
//
// H3 conflict handling: if the state has been independently transitioned to a
// different terminal status (e.g., dead-letter set it to failed while a
// previous Complete crashed mid-sequence), the result/queue artifacts written
// by the previous attempt will be inconsistent with state. Rather than
// silently skipping, we reconcile result/queue forward to match the actual
// state.PlanStatus so all three stores agree.
func executeCompleteSteps(opts CompleteOptions, sm *StateManager, state *model.CommandState, intent *completeIntent) error {
	// Conflict path: state already terminal AND differs from intent.
	if model.IsPlanTerminal(state.PlanStatus) && state.PlanStatus != intent.PlanStatus {
		actualStatus, err := planStatusToResultStatus(state.PlanStatus)
		if err != nil {
			return fmt.Errorf("conflict reconcile: %w", err)
		}
		log.Printf("[WARN] executeCompleteSteps: conflict — state=%s intent=%s for command %s; reconciling result/queue to state",
			state.PlanStatus, intent.PlanStatus, intent.CommandID)

		opts.LockMap.Lock("result:planner")
		rerr := reconcileCommandResultLocked(opts.MaestroDir, intent.CommandID, actualStatus, intent.Summary, intent.TaskResults)
		opts.LockMap.Unlock("result:planner")
		if rerr != nil {
			return fmt.Errorf("reconcile command result: %w", rerr)
		}
		if err := reconcileCommandQueueEntryLocked(opts.MaestroDir, intent.CommandID, actualStatus); err != nil {
			return fmt.Errorf("reconcile command queue: %w", err)
		}
		return nil
	}

	// Step 1: Write to results/planner.yaml (narrow lock scope for result:planner)
	opts.LockMap.Lock("result:planner")
	err := writeCommandResultLocked(opts.MaestroDir, intent.CommandID, intent.ResultStatus, intent.Summary, intent.TaskResults)
	opts.LockMap.Unlock("result:planner")
	if err != nil {
		return fmt.Errorf("write command result: %w", err)
	}

	// Step 2: Update queue/planner.yaml command entry (caller holds queue:planner)
	if err := updateCommandQueueEntryLocked(opts.MaestroDir, intent.CommandID, intent.ResultStatus); err != nil {
		return fmt.Errorf("update command queue: %w", err)
	}

	// Step 3: Update state plan_status (caller holds state:{commandID})
	// Idempotent: state matches intent already, no-op. Otherwise update.
	if state.PlanStatus != intent.PlanStatus {
		state.PlanStatus = intent.PlanStatus
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := sm.SaveState(state); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}

	return nil
}

// planStatusToResultStatus maps a terminal PlanStatus to the corresponding
// command-level result Status.
func planStatusToResultStatus(ps model.PlanStatus) (model.Status, error) {
	switch ps {
	case model.PlanStatusCompleted:
		return model.StatusCompleted, nil
	case model.PlanStatusFailed:
		return model.StatusFailed, nil
	case model.PlanStatusCancelled:
		return model.StatusCancelled, nil
	default:
		return "", fmt.Errorf("not a terminal plan status: %s", ps)
	}
}

// reconcileCommandResultLocked overwrites an existing command result entry's
// status (preserving the result ID so downstream notification dedup keys
// remain stable) when an H3 conflict is detected. If no entry exists yet,
// a new one is appended via writeCommandResultLocked.
// Precondition: caller holds "result:planner" lock.
func reconcileCommandResultLocked(maestroDir string, commandID string, status model.Status, summary string, tasks []model.CommandResultTask) error {
	path := filepath.Join(maestroDir, "results", "planner.yaml")

	var rf model.CommandResultFile
	data, err := os.ReadFile(path)
	if err == nil {
		if perr := yamlv3.Unmarshal(data, &rf); perr != nil {
			return fmt.Errorf("parse existing result file: %w", perr)
		}
	}
	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_command"
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := range rf.Results {
		if rf.Results[i].CommandID == commandID {
			// Preserve ID; mutate status/summary/tasks. Reset Notified so
			// the orchestrator notification path can resend the corrected
			// result. Pending notifications referencing the original ID
			// remain valid because the ID itself is unchanged.
			rf.Results[i].Status = status
			rf.Results[i].Summary = summary
			rf.Results[i].Tasks = tasks
			rf.Results[i].Notified = false
			rf.Results[i].CreatedAt = now
			return yamlutil.AtomicWrite(path, rf)
		}
	}

	// No existing entry: fall back to a fresh append.
	resultID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return fmt.Errorf("generate result ID: %w", err)
	}
	rf.Results = append(rf.Results, model.CommandResult{
		ID:        resultID,
		CommandID: commandID,
		Status:    status,
		Summary:   summary,
		Tasks:     tasks,
		Notified:  false,
		CreatedAt: now,
	})
	return yamlutil.AtomicWrite(path, rf)
}

// reconcileCommandQueueEntryLocked force-updates a command's status in
// queue/planner.yaml even if the entry is already in a terminal state. This
// is the H3 reconciliation counterpart to updateCommandQueueEntryLocked,
// which is intentionally idempotent (no-op on terminal). If the entry is
// missing (already archived), this is a no-op.
// Precondition: caller holds "queue:planner" lock.
func reconcileCommandQueueEntryLocked(maestroDir string, commandID string, status model.Status) error {
	path := filepath.Join(maestroDir, "queue", "planner.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read planner queue: %w", err)
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			if cq.Commands[i].Status == status {
				return nil
			}
			cq.Commands[i].Status = status
			cq.Commands[i].LeaseOwner = nil
			cq.Commands[i].LeaseExpiresAt = nil
			cq.Commands[i].UpdatedAt = now
			return yamlutil.AtomicWrite(path, cq)
		}
	}

	log.Printf("[WARN] reconcileCommandQueueEntryLocked: command %s not found in planner queue (already archived)", commandID)
	return nil
}

// replayCompleteIntent recovers a stale intent by re-running all 3 steps.
// Returns the actual final plan_status (which may differ from the intent's
// plan_status if the state was independently transitioned to a different
// terminal status).
func replayCompleteIntent(opts CompleteOptions, sm *StateManager, intent *completeIntent) (model.PlanStatus, error) {
	state, err := sm.LoadState(intent.CommandID)
	if err != nil {
		return "", fmt.Errorf("load state for recovery: %w", err)
	}

	// If state is already terminal, steps 1+2 may already be done; replay is
	// still safe because each step is idempotent.
	if err := executeCompleteSteps(opts, sm, state, intent); err != nil {
		return "", err
	}

	removeCompleteIntent(opts.MaestroDir, intent.CommandID)

	// Return the actual state status (may differ from intent if state was
	// transitioned independently, e.g. via cancel).
	if model.IsPlanTerminal(state.PlanStatus) {
		return state.PlanStatus, nil
	}
	return intent.PlanStatus, nil
}

// --- Intent file helpers (CR-019) ---

func completeIntentPath(maestroDir, commandID string) string {
	return filepath.Join(maestroDir, "intents", "complete_"+commandID+".yaml")
}

func writeCompleteIntent(maestroDir string, intent *completeIntent) error {
	dir := filepath.Join(maestroDir, "intents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create intents dir: %w", err)
	}
	return yamlutil.AtomicWrite(completeIntentPath(maestroDir, intent.CommandID), intent)
}

func readCompleteIntent(maestroDir, commandID string) (*completeIntent, error) {
	path := completeIntentPath(maestroDir, commandID)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var intent completeIntent
	if err := yamlv3.Unmarshal(data, &intent); err != nil {
		return nil, fmt.Errorf("parse intent: %w", err)
	}
	// Structural validation (including status fields to prevent invalid values
	// from being written to result/queue/state files during replay).
	if intent.SchemaVersion != 1 || intent.FileType != "intent_plan_complete" || intent.CommandID == "" {
		return nil, fmt.Errorf("invalid intent: schema_version=%d file_type=%q command_id=%q",
			intent.SchemaVersion, intent.FileType, intent.CommandID)
	}
	if !model.IsTerminal(intent.ResultStatus) {
		return nil, fmt.Errorf("invalid intent: result_status %q is not terminal", intent.ResultStatus)
	}
	if !model.IsPlanTerminal(intent.PlanStatus) {
		return nil, fmt.Errorf("invalid intent: plan_status %q is not terminal", intent.PlanStatus)
	}
	return &intent, nil
}

func removeCompleteIntent(maestroDir, commandID string) {
	_ = os.Remove(completeIntentPath(maestroDir, commandID))
}

func aggregateTaskResults(maestroDir string, commandID string) ([]model.CommandResultTask, bool, error) {
	resultsDir := filepath.Join(maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil, false, fmt.Errorf("read results dir: %w", err)
	}

	var taskResults []model.CommandResultTask
	partial := false

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[WARN] aggregateTaskResults: failed to read %s: %v (results may be partial)", path, err)
			partial = true
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			log.Printf("[WARN] aggregateTaskResults: failed to parse %s: %v (results may be partial)", path, err)
			partial = true
			continue
		}

		for _, r := range rf.Results {
			if r.CommandID == commandID {
				taskResults = append(taskResults, model.CommandResultTask{
					TaskID:  r.TaskID,
					Worker:  workerID,
					Status:  r.Status,
					Summary: r.Summary,
				})
			}
		}
	}

	return taskResults, partial, nil
}

// writeCommandResultLocked writes a command result to results/planner.yaml.
// Precondition: caller holds "result:planner" lock.
func writeCommandResultLocked(maestroDir string, commandID string, status model.Status, summary string, tasks []model.CommandResultTask) error {
	path := filepath.Join(maestroDir, "results", "planner.yaml")

	var rf model.CommandResultFile
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			return fmt.Errorf("parse existing result file: %w", err)
		}
	}
	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_command"
	}

	// Idempotency: skip if a result for this commandID already exists
	for _, existing := range rf.Results {
		if existing.CommandID == commandID {
			return nil
		}
	}

	resultID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return fmt.Errorf("generate result ID: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rf.Results = append(rf.Results, model.CommandResult{
		ID:        resultID,
		CommandID: commandID,
		Status:    status,
		Summary:   summary,
		Tasks:     tasks,
		Notified:  false,
		CreatedAt: now,
	})

	return yamlutil.AtomicWrite(path, rf)
}

// updateCommandQueueEntryLocked updates a command's status in queue/planner.yaml.
// Precondition: caller holds "queue:planner" lock.
// Idempotent: if the command is already in a terminal status, this is a no-op
// (safe for crash-recovery replay).
func updateCommandQueueEntryLocked(maestroDir string, commandID string, status model.Status) error {
	path := filepath.Join(maestroDir, "queue", "planner.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read planner queue: %w", err)
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	found := false
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			// Idempotent: already terminal → no-op (recovery replay safe)
			if model.IsTerminal(cq.Commands[i].Status) {
				return nil
			}
			cq.Commands[i].Status = status
			cq.Commands[i].LeaseOwner = nil
			cq.Commands[i].LeaseExpiresAt = nil
			cq.Commands[i].UpdatedAt = now
			found = true
			break
		}
	}

	if !found {
		// Command may have been archived after a previous partial completion;
		// treat as already handled (recovery safe).
		log.Printf("[WARN] updateCommandQueueEntryLocked: command %s not found in planner queue (may be archived)", commandID)
		return nil
	}

	return yamlutil.AtomicWrite(path, cq)
}

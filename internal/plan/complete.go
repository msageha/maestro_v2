package plan

import (
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

type CompleteOptions struct {
	CommandID  string
	Summary    string
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
}

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
func executeCompleteSteps(opts CompleteOptions, sm *StateManager, state *model.CommandState, intent *completeIntent) error {
	// Early exit: if state has already been transitioned to a different terminal
	// status by another process (e.g., dead-letter, cancel), skip all steps to
	// prevent writing result/queue artifacts with an inconsistent status.
	// If state matches the intent's terminal status, proceed to ensure all
	// artifacts are consistent (partial-replay scenario).
	if model.IsPlanTerminal(state.PlanStatus) && state.PlanStatus != intent.PlanStatus {
		log.Printf("[WARN] executeCompleteSteps: state already terminal (%s), differs from intent (%s) for command %s; skipping all steps",
			state.PlanStatus, intent.PlanStatus, intent.CommandID)
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
	// Idempotent: skip if state is already in the target terminal status.
	if state.PlanStatus != intent.PlanStatus {
		if model.IsPlanTerminal(state.PlanStatus) {
			// State reached a different terminal status (e.g. via cancel); do not
			// overwrite. Log and treat as success for the completion sequence.
			log.Printf("[WARN] executeCompleteSteps: state already terminal (%s) but differs from intent (%s) for command %s",
				state.PlanStatus, intent.PlanStatus, intent.CommandID)
		} else {
			state.PlanStatus = intent.PlanStatus
			state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := sm.SaveState(state); err != nil {
				return fmt.Errorf("save state: %w", err)
			}
		}
	}

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
		if os.IsNotExist(err) {
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

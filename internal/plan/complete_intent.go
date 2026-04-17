package plan

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// intentSchemaVersion is the current schema version for complete intent files.
const intentSchemaVersion = 1

// completeIntent is the WAL record written before the multi-step completion
// sequence. If a crash occurs mid-sequence, the next Complete() call for the
// same command will discover the intent and replay all steps idempotently
// (CR-019). Note: intents for other commands are not recovered here; daemon
// startup should scan the intents directory for broader recovery.
type completeIntent struct {
	SchemaVersion       int                       `yaml:"schema_version"`
	FileType            string                    `yaml:"file_type"`
	CommandID           string                    `yaml:"command_id"`
	Summary             string                    `yaml:"summary"`
	ResultStatus        model.Status              `yaml:"result_status"`
	PlanStatus          model.PlanStatus          `yaml:"plan_status"`
	TaskResults         []model.CommandResultTask `yaml:"task_results"`
	CreatedAt           string                    `yaml:"created_at"`
	ProcessingStartedAt string                    `yaml:"processing_started_at,omitempty"`
}

// replayCompleteIntent recovers a stale intent by re-running all 3 steps.
// Returns the actual final plan_status (which may differ from the intent's
// plan_status if the state was independently transitioned to a different
// terminal status).
//
// Precondition: caller holds both queue:planner and state:{commandID} locks,
// which blocks new result writes and state mutations for the duration of
// recovery (C-A2).
func replayCompleteIntent(opts CompleteOptions, sm *StateManager, intent *completeIntent) (model.PlanStatus, error) {
	// C-A2: Detect abandoned recovery — if ProcessingStartedAt is already set,
	// a previous recovery attempt crashed before cleanup.
	if intent.ProcessingStartedAt != "" {
		slog.Warn("replayCompleteIntent: previous processing attempt detected",
			"command_id", intent.CommandID,
			"previous_started_at", intent.ProcessingStartedAt)
	}

	// Stamp processing_started_at so concurrent observers (e.g. daemon startup
	// scanner) can detect a stale/abandoned recovery.
	intent.ProcessingStartedAt = nowUTC()
	if err := writeCompleteIntent(opts.MaestroDir, intent); err != nil {
		return "", fmt.Errorf("stamp intent processing: %w", err)
	}

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
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for an intents directory
		return fmt.Errorf("create intents dir: %w", err)
	}
	return yamlutil.AtomicWrite(completeIntentPath(maestroDir, intent.CommandID), intent)
}

func readCompleteIntent(maestroDir, commandID string) (*completeIntent, error) {
	path := completeIntentPath(maestroDir, commandID)
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
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
	if intent.SchemaVersion != intentSchemaVersion || intent.FileType != "intent_plan_complete" || intent.CommandID == "" {
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

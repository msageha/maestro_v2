package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const deferredCompleteSchemaVersion = 1

// deferredComplete stores a plan complete request that was deferred because
// the worktree publish hadn't completed yet. When publish succeeds, the daemon
// reads this file and auto-completes the plan using the stored summary.
type deferredComplete struct {
	SchemaVersion int    `yaml:"schema_version"`
	FileType      string `yaml:"file_type"`
	CommandID     string `yaml:"command_id"`
	Summary       string `yaml:"summary"`
	CreatedAt     string `yaml:"created_at"`
}

func deferredCompletePath(maestroDir, commandID string) string {
	return filepath.Join(maestroDir, "intents", "deferred_complete_"+commandID+".yaml")
}

// WriteDeferredComplete writes a deferred plan complete intent file.
// Subsequent calls for the same commandID overwrite the previous intent.
func WriteDeferredComplete(maestroDir, commandID, summary string) error {
	dir := filepath.Join(maestroDir, "intents")
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for an intents directory
		return fmt.Errorf("create intents dir: %w", err)
	}
	dc := &deferredComplete{
		SchemaVersion: deferredCompleteSchemaVersion,
		FileType:      "deferred_plan_complete",
		CommandID:     commandID,
		Summary:       summary,
		CreatedAt:     nowUTC(),
	}
	return yamlutil.AtomicWrite(deferredCompletePath(maestroDir, commandID), dc)
}

// readDeferredComplete reads a deferred plan complete intent. Returns (nil, nil)
// if the file does not exist. Package-internal only — the type returned is
// unexported because callers outside this package use CompleteDeferredPublish.
func readDeferredComplete(maestroDir, commandID string) (*deferredComplete, error) {
	path := deferredCompletePath(maestroDir, commandID)
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var dc deferredComplete
	if err := yamlutil.SafeUnmarshal(data, &dc); err != nil {
		return nil, fmt.Errorf("parse deferred complete: %w", err)
	}
	if dc.SchemaVersion != deferredCompleteSchemaVersion || dc.FileType != "deferred_plan_complete" || dc.CommandID == "" {
		return nil, fmt.Errorf("invalid deferred complete: schema_version=%d file_type=%q command_id=%q",
			dc.SchemaVersion, dc.FileType, dc.CommandID)
	}
	return &dc, nil
}

// RemoveDeferredComplete removes a deferred plan complete intent file.
func RemoveDeferredComplete(maestroDir, commandID string) {
	_ = os.Remove(deferredCompletePath(maestroDir, commandID))
}

// CompleteDeferredPublish reads a deferred plan complete intent and, if found,
// calls Complete() with the stored summary. Returns (nil, nil) when no deferred
// intent exists for the given commandID.
func CompleteDeferredPublish(opts CompleteOptions) (*CompleteResult, error) {
	dc, err := readDeferredComplete(opts.MaestroDir, opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("read deferred complete: %w", err)
	}
	if dc == nil {
		return nil, nil
	}

	opts.Summary = dc.Summary
	res, err := Complete(opts)
	if err != nil {
		// Keep the intent file so the next scan tick retries. Removing it
		// before a successful Complete() (the previous behaviour) stranded the
		// command on any transient failure — a momentary result/state I/O error
		// or a stale-results conflict dropped the deferred intent permanently,
		// leaving plan_status non-terminal and the orchestrator un-notified
		// (the exact P0 wedge stepFinalizeQuarantinedDeferredComplete exists to
		// resolve, whose comment already assumes the intent survives on error).
		return res, err
	}

	// Complete() can legitimately re-defer (publish still not finished): it
	// writes a fresh deferred intent and returns Status="deferred_publish".
	// Removing the intent in that case would delete the just-written one and
	// strand completion, so only remove it once Complete has actually finalised
	// the command. At that point publish is done, so Complete does not write a
	// new intent and there is no infinite-loop risk.
	if res == nil || res.Status != "deferred_publish" {
		RemoveDeferredComplete(opts.MaestroDir, opts.CommandID)
	}
	return res, nil
}
